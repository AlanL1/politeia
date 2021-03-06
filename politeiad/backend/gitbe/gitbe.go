package gitbe

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/decred/dcrd/chaincfg"
	"github.com/decred/dcrtime/api/v1"
	"github.com/decred/dcrtime/merkle"
	"github.com/decred/politeia/decredplugin"
	pd "github.com/decred/politeia/politeiad/api/v1"
	"github.com/decred/politeia/politeiad/api/v1/identity"
	"github.com/decred/politeia/politeiad/api/v1/mime"
	"github.com/decred/politeia/politeiad/backend"
	"github.com/decred/politeia/util"
	"github.com/marcopeereboom/lockfile"
	"github.com/robfig/cron"
	"github.com/subosito/norma"
	"github.com/syndtr/goleveldb/leveldb"
)

const (
	// Lockfile is the filesystem lock filename.  Export for external utilities.
	LockFilename = ".lock"

	// LockDuration is the maximum lock time duration allowed.  15 seconds
	// is ~3x of anchoring without internet delay.
	LockDuration = 15 * time.Second

	// defaultUnvettedPath is the landing zone for unvetted content.
	defaultUnvettedPath = "unvetted"

	// defaultVettedPath is the publicly visible git vetted record repo.
	defaultVettedPath = "vetted"

	// defaultRecordMetadataFilename is the filename of record record.
	defaultRecordMetadataFilename = "recordmetadata.json"

	// defaultMDFilenameSuffix is the filename suffic for the user provided
	// metadata record.  The metadata record shall be string encoded.
	defaultMDFilenameSuffix = ".metadata.txt"

	// defaultAuditTrailFile is the filename where a human readable audit
	// trail is kept.
	defaultAuditTrailFile = "anchor_audit_trail.txt"

	// defaultAnchorsDirectory is the directory where anchors are stored.
	// They are indexed by TX.
	defaultAnchorsDirectory = "anchors"

	// defaultPayloadDir is the default path to store a record payload.
	defaultPayloadDir = "payload"

	// anchorSchedule determines how often we anchor the vetted repo.
	// Seconds Minutes Hours Days Months DayOfWeek
	anchorSchedule = "0 58 * * * *" // At 58 minutes every hour

	// expectedTestTX is a fake TX used by unit tests.
	expectedTestTX = "TESTTX"

	// markerAnchor is used in commit messages to determine
	// where an anchor has been committed.  This value is
	// parsed and therefore must be a const.
	markerAnchor = "Anchor"

	// markerAnchorConfirmation is used in commit messages to determine
	// where an anchor confirmation has been committed.  This value is
	// parsed and therefore must be a const.
	markerAnchorConfirmation = "Anchor confirmation"
)

var (
	_ backend.Backend = (*gitBackEnd)(nil)

	defaultRepoConfig = map[string]string{
		// This prevents git from converting CRLF when committing and checking
		// out files, which helps when running on Windows.
		"core.autocrlf": "false",
	}

	errNothingToDo = errors.New("nothing to do")
)

// file is an internal representation of a file that resides in memory.
type file struct {
	name    string // Basename of the file
	digest  []byte // SHA256 of payload
	payload []byte // Actual file payload
}

// gitBackEnd is a git based backend context that satisfies the backend
// interface.
type gitBackEnd struct {
	lock            *lockfile.LockFile // Global lock
	db              *leveldb.DB        // Database
	cron            *cron.Cron         // Scheduler for periodic tasks
	activeNetParams *chaincfg.Params   // indicator if we are running on testnet
	shutdown        bool               // Backend is shutdown
	root            string             // Root directory
	unvetted        string             // Unvettend content
	vetted          string             // Vetted, public, visible content
	dcrtimeHost     string             // Dcrtimed directory
	gitPath         string             // Path to git
	gitTrace        bool               // Enable git tracing
	test            bool               // Set during UT
	exit            chan struct{}      // Close channel
	checkAnchor     chan struct{}      // Work notification
	plugins         []backend.Plugin   // Plugins

	// The following items are used for testing only
	testAnchors map[string]bool // [digest]anchored
}

// extendSHA1 appends 0 to make a SHA1 the size of a SHA256 digest.
func extendSHA1(d []byte) []byte {
	if len(d) != sha1.Size {
		panic("invalid sha1 length")
	}
	digest := make([]byte, sha256.Size)
	copy(digest, d)
	return digest
}

// unextendSHA1ToSha256 removes 0 to make a SHA256 the size of a SHA1 digest.
func unextendSHA256(d []byte) []byte {
	if len(d) != sha256.Size {
		panic("invalid sha256 length")
	}
	// make sure this was an extended digest
	for _, x := range d[sha1.Size:] {
		if x != 0 {
			panic("invalid extended sha256")
		}
	}
	digest := make([]byte, sha1.Size)
	copy(digest, d)
	return digest
}

// extendSHA1FromString takes a string and ensures it is a digest and then
// extends it using extendSHA1.  It returns a string representation of the
// digest.
func extendSHA1FromString(s string) (string, error) {
	ds, err := hex.DecodeString(s)
	if err != nil {
		return "", fmt.Errorf("not hex: %v", s)
	}
	d := extendSHA1(ds)
	return hex.EncodeToString(d), nil
}

// newUniqueID returns a new unique record ID.  The function will hold the
// unvettedLock if successful.  The callee is responsible for releasing the
// lock.
//
// This function must be called without holding the unvetted lock.
func (g *gitBackEnd) newUniqueID() (uint64, error) {
	err := g.lock.Lock(LockDuration)
	if err != nil {
		return 0, err
	}

	// Get Dirs.
	files, err := ioutil.ReadDir(g.unvetted)
	if err != nil {
		return 0, err
	}

	// Find biggest record ID
	var last uint64
	for _, file := range files {
		// This check ignores lockFilename as well
		if !file.IsDir() {
			continue
		}
		p, err := strconv.ParseUint(file.Name(), 10, 64)
		if err != nil {
			continue
		}
		if p > last {
			last = p
		}
	}
	id := last + 1

	// Create directory
	err = os.MkdirAll(filepath.Join(g.unvetted, strconv.FormatUint(id, 10)),
		0774)
	if err != nil {
		return 0, err
	}

	return id, nil
}

// verifyContent verifies that all provided backend.MetadataStream and
// backend.File are sane and returns a cooked array of the files.
func verifyContent(metadata []backend.MetadataStream, files []backend.File, filesDel []string) ([]file, error) {
	// Make sure all metadata is within maxima.
	for _, v := range metadata {
		if v.ID > pd.MetadataStreamsMax-1 {
			return nil, backend.ContentVerificationError{
				ErrorCode: pd.ErrorStatusInvalidMDID,
				ErrorContext: []string{
					strconv.FormatUint(v.ID, 10),
				},
			}
		}
	}
	for i := range metadata {
		for j := range metadata {
			// Skip self and non duplicates.
			if i == j || metadata[i].ID != metadata[j].ID {
				continue
			}
			return nil, backend.ContentVerificationError{
				ErrorCode: pd.ErrorStatusDuplicateMDID,
				ErrorContext: []string{
					strconv.FormatUint(metadata[i].ID, 10),
				},
			}
		}
	}

	// Prevent paths
	for i := range files {
		if filepath.Base(files[i].Name) != files[i].Name {
			return nil, backend.ContentVerificationError{
				ErrorCode: pd.ErrorStatusInvalidFilename,
				ErrorContext: []string{
					files[i].Name,
				},
			}
		}
	}
	for _, v := range filesDel {
		if filepath.Base(v) != v {
			return nil, backend.ContentVerificationError{
				ErrorCode: pd.ErrorStatusInvalidFilename,
				ErrorContext: []string{
					v,
				},
			}
		}
	}

	// Now check files
	if len(files) == 0 {
		return nil, backend.ContentVerificationError{
			ErrorCode: pd.ErrorStatusEmpty,
		}
	}

	// Prevent bad filenames and duplicate filenames
	for i := range files {
		for j := range files {
			if i == j {
				continue
			}
			if files[i].Name == files[j].Name {
				return nil, backend.ContentVerificationError{
					ErrorCode: pd.ErrorStatusDuplicateFilename,
					ErrorContext: []string{
						files[i].Name,
					},
				}
			}
		}
		// Check against filesDel
		for _, v := range filesDel {
			if files[i].Name == v {
				return nil, backend.ContentVerificationError{
					ErrorCode: pd.ErrorStatusDuplicateFilename,
					ErrorContext: []string{
						files[i].Name,
					},
				}
			}
		}
	}

	fa := make([]file, 0, len(files))
	for i := range files {
		if norma.Sanitize(files[i].Name) != files[i].Name {
			return nil, backend.ContentVerificationError{
				ErrorCode: pd.ErrorStatusInvalidFilename,
				ErrorContext: []string{
					files[i].Name,
				},
			}
		}

		// Validate digest
		d, ok := util.ConvertDigest(files[i].Digest)
		if !ok {
			return nil, backend.ContentVerificationError{
				ErrorCode: pd.ErrorStatusInvalidFileDigest,
				ErrorContext: []string{
					files[i].Name,
				},
			}
		}

		// Setup cooked file.
		f := file{
			name: files[i].Name,
		}

		// Decode base64 payload
		var err error
		f.payload, err = base64.StdEncoding.DecodeString(files[i].Payload)
		if err != nil {
			return nil, backend.ContentVerificationError{
				ErrorCode: pd.ErrorStatusInvalidBase64,
				ErrorContext: []string{
					files[i].Name,
				},
			}
		}

		// Calculate payload digest
		dp := util.Digest(f.payload)
		if !bytes.Equal(d[:], dp) {
			return nil, backend.ContentVerificationError{
				ErrorCode: pd.ErrorStatusInvalidFileDigest,
				ErrorContext: []string{
					files[i].Name,
				},
			}
		}
		f.digest = dp

		// Verify MIME
		detectedMIMEType := http.DetectContentType(f.payload)
		if detectedMIMEType != files[i].MIME {
			return nil, backend.ContentVerificationError{
				ErrorCode: pd.ErrorStatusInvalidMIMEType,
				ErrorContext: []string{
					files[i].Name,
					detectedMIMEType,
				},
			}
		}
		if !mime.MimeValid(files[i].MIME) {
			return nil, backend.ContentVerificationError{
				ErrorCode: pd.ErrorStatusUnsupportedMIMEType,
				ErrorContext: []string{
					files[i].Name,
					files[i].MIME,
				},
			}
		}

		fa = append(fa, f)
	}

	return fa, nil
}

// loadRecord loads an entire record of disk.  It returns an array of
// backend.File that is completely filled out.
//
// This function must be called with the lock held.
func loadRecord(path, id string) ([]backend.File, error) {
	// Get dir.
	recordDir := filepath.Join(path, id, defaultPayloadDir)
	files, err := ioutil.ReadDir(recordDir)
	if err != nil {
		return nil, err
	}

	bf := make([]backend.File, 0, len(files))
	// Load all files
	for _, file := range files {
		fn := filepath.Join(recordDir, file.Name())
		if file.IsDir() {
			return nil, fmt.Errorf("record corrupt: %v", path)
		}

		f := backend.File{Name: file.Name()}
		f.MIME, f.Digest, f.Payload, err = util.LoadFile(fn)
		if err != nil {
			return nil, err
		}
		bf = append(bf, f)
	}

	return bf, nil
}

// mdFilename generates the proper filename for a specified repo + proposal and
// metadata stream.
func mdFilename(path, id string, mdID int) string {
	return filepath.Join(path, id, strconv.FormatUint(uint64(mdID), 10)+
		defaultMDFilenameSuffix)
}

// loadMDStreams loads all streams of disk.  It returns an array of
// backend.MetadataStream that is completely filled out.
//
// This function must be called with the lock held.
func loadMDStreams(path, id string) ([]backend.MetadataStream, error) {
	// Get dir.
	dir := filepath.Join(path, id)
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	ms := make([]backend.MetadataStream, 0, len(files))
	for _, v := range files {
		// Skip irrelevant files
		if !strings.HasSuffix(v.Name(), defaultMDFilenameSuffix) {
			continue
		}

		// Fish out metadata stream ID from filename
		ids := strings.TrimSuffix(v.Name(), defaultMDFilenameSuffix)
		mdid, err := strconv.ParseUint(ids, 10, 64)
		if err != nil {
			return nil, err
		}

		// Load metadata stream
		fn := filepath.Join(dir, v.Name())
		md, err := ioutil.ReadFile(fn)
		if err != nil {
			return nil, err
		}
		ms = append(ms, backend.MetadataStream{
			ID:      mdid,
			Payload: string(md),
		})
	}

	return ms, nil
}

// loadMD loads a RecordMetadata from the provided path/id.  This may
// be unvetted/id or vetted/id.
//
// This function should be called with the lock held.
func loadMD(path, id string) (*backend.RecordMetadata, error) {
	filename := filepath.Join(path, id,
		defaultRecordMetadataFilename)
	f, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			err = backend.ErrRecordNotFound
		}
		return nil, err
	}
	defer f.Close()

	var brm backend.RecordMetadata
	decoder := json.NewDecoder(f)
	if err = decoder.Decode(&brm); err != nil {
		return nil, err
	}
	return &brm, nil
}

// createMD stores a RecordMetadata to the provided path/id.  This may be
// unvetted/id or vetted/id.
//
// This function should be called with the lock held.
func createMD(path, id string, status backend.MDStatusT, version uint, hashes []*[sha256.Size]byte, token []byte) (*backend.RecordMetadata, error) {
	// Create record metadata
	brm := backend.RecordMetadata{
		Version:   version,
		Status:    status,
		Merkle:    *merkle.Root(hashes),
		Timestamp: time.Now().Unix(),
		Token:     token,
	}

	err := updateMD(path, id, &brm)
	if err != nil {
		return nil, err
	}

	return &brm, nil
}

// updateMD updates the RecordMetadata status to the provided path/id.
//
// This function should be called with the lock held.
func updateMD(path, id string, brm *backend.RecordMetadata) error {
	// Store metadata record.
	filename := filepath.Join(path, id, defaultRecordMetadataFilename)
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	return json.NewEncoder(f).Encode(*brm)
}

// commitMD commits the MD into a git repo.
//
// This function should be called with the lock held.
func (g *gitBackEnd) commitMD(path, id, msg string) error {
	// git add id/brm.json
	filename := filepath.Join(path, id,
		defaultRecordMetadataFilename)
	err := g.gitAdd(path, filename)
	if err != nil {
		return err
	}

	// git commit -m "message"
	return g.gitCommit(path, "Update record status "+id+" "+msg)
}

// deltaCommits returns sha1 extended digests and one line commit messages to
// the caller.  If lastAnchor is empty then the range is from the dawn of time
// until now.  If lastAnchor is a valid hash the range is from lastAnchor up
// until no.
//
// This function should be called with the lock held.
func (g *gitBackEnd) deltaCommits(path string, lastAnchor []byte) ([]*[sha256.Size]byte, []string, []string, error) {
	// Sanity
	if !(len(lastAnchor) == 0 || len(lastAnchor) == sha256.Size) {
		return nil, nil, nil, fmt.Errorf("invalid digest size")
	}

	// Minimal git arguments
	args := []string{"log", "--pretty=oneline"}

	// Determine digest range
	latestCommit, err := g.gitLastDigest(path)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(lastAnchor) != 0 {
		// git log lastAnchor..latestCommit --pretty=oneline
		sha1LastAnchor := unextendSHA256(lastAnchor)
		if bytes.Equal(sha1LastAnchor, latestCommit) {
			return nil, nil, nil, errNothingToDo
		}
		args = append(args, hex.EncodeToString(sha1LastAnchor)+".."+
			hex.EncodeToString(latestCommit))
	}

	// Execute git
	out, err := g.git(path, args...)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(out) == 0 {
		return nil, nil, nil, fmt.Errorf("invalid git output")
	}

	// Generate return data
	digests := make([]*[sha256.Size]byte, 0, len(out))
	commitMessages := make([]string, 0, len(out))
	for _, line := range out {
		// Returned data is "<digest> <commit message>"
		ds := strings.SplitN(line, " ", 2)
		if len(ds) == 0 {
			return nil, nil, nil, fmt.Errorf("invalid log")
		}

		// Ignore anchor confirmation commits
		if regexAnchorConfirmation.MatchString(ds[1]) {
			continue
		}

		// Validate returned digest
		sha1Digest, err := hex.DecodeString(ds[0])
		if err != nil {
			return nil, nil, nil, err
		}
		if len(sha1Digest) != sha1.Size {
			return nil, nil, nil, fmt.Errorf("invalid sha1 size")
		}
		sha256DigestB := extendSHA1(sha1Digest)
		var sha256Digest [sha256.Size]byte
		copy(sha256Digest[:], sha256DigestB)

		// Fill out return values
		digests = append(digests, &sha256Digest)
		commitMessages = append(commitMessages, ds[1])
	}

	if len(digests) == 0 {
		return nil, nil, nil, errNothingToDo
	}

	return digests, commitMessages, out, nil
}

// anchor takes a slice of commit digests and anchors them in dcrtime.
//
// This function is being clever with the anchors.  It sends two values to
// dcrtime.  We anchor the merkle root, and we *also* anchor all
// individual commit hashes.  We do the last bit in order to be able to
// externally validate that a commit hash made it into the time stamp.  If we
// don't do that we'd have to create a tool to verify individual hashes for the
// truly curious.  This is essentially free because dcrtime compresses all
// digests into a single merkle root.
//
// This function should be called with the lock held.
// TODO: the physical write to dcrtime needs to come out of the lock.
func (g *gitBackEnd) anchor(digests []*[sha256.Size]byte) error {
	// Anchor all digests
	if g.test {
		// We always append the anchorKey as the last element
		x := len(digests) - 1
		g.testAnchors[hex.EncodeToString(digests[x][:])] = false
		return nil
	}

	return util.Timestamp(g.dcrtimeHost, digests)
}

// appendAuditTrail adds a record to the audit trail.
func (g *gitBackEnd) appendAuditTrail(path string, ts int64, merkle [sha256.Size]byte, lines []string) error {
	f, err := os.OpenFile(filepath.Join(path, defaultAuditTrailFile),
		os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	fmt.Fprintf(f, "%v: --- Audit Trail Record %x ---\n", ts, merkle)
	for _, line := range lines {
		fmt.Fprintf(f, "%v: %v\n", ts, strings.Trim(line, " \t\n"))
	}

	return nil
}

// anchorRepo drops an anchor for an individual repo.
// It prints the basename during its actions.
//
// This function should be called with the lock held.
func (g *gitBackEnd) anchorRepo(path string) (*[sha256.Size]byte, error) {
	// Make sure we have a repo we understand
	repo := filepath.Base(path)

	// Fsck
	log.Infof("Running git fsck on %v repository", repo)
	err := g.gitCheckout(path, "master")
	if err != nil {
		return nil, fmt.Errorf("anchor checkout master %v: %v", repo,
			err)
	}
	_, err = g.gitFsck(path)
	if err != nil {
		return nil, fmt.Errorf("anchor fsck master %v: %v", repo, err)
	}

	// Check for unanchored commits
	last, err := g.readLastAnchorRecord()
	if err != nil {
		return nil, fmt.Errorf("could not find last %v digest: %v", repo,
			err)
	}

	// Fill out unvetted digests
	digests, messages, _, err := g.deltaCommits(path, last.Last)
	if err != nil {
		if err == errNothingToDo {
			return nil, err
		}
		return nil, fmt.Errorf("could not determine delta %v: %v",
			repo, err)
	}
	if len(digests) != len(messages) {
		// Really can't happen
		return nil, fmt.Errorf("invalid digests(%v)/messages(%v) count",
			len(digests), len(messages))
	}

	// Create commit message BEFORE calling anchor.  anchor calls
	// merkle.Root which in turn sorts the digests and that is fine but not
	// what we want to display to the user.
	commitMessage := ""
	auditLines := make([]string, 0, len(digests))
	for k, digest := range digests {
		line := fmt.Sprintf("%x %v\n", *digest, messages[k])
		commitMessage += line
		auditLines = append(auditLines, line)
	}

	// Create anchor record early for the same reason.
	anchorRecord, anchorKey, err := newAnchorRecord(AnchorUnverified,
		digests, messages)
	if err != nil {
		return nil, fmt.Errorf("newAnchorRecord: %v", err)
	}

	// Append MerkleRoot to digests.  We have to do this since this is
	// politeia's lookup key but dcrtime will likely return a different
	// merkle.  Dcrtime returns a different merkle when there are
	// additional digests in the set.
	digests = append(digests, anchorKey)

	// Anchor commits
	log.Infof("Anchoring %v repository", repo)
	err = g.anchor(digests)
	if err != nil {
		return nil, fmt.Errorf("anchor: %v", err)
	}

	// Prefix commitMessage with merkle root
	commitMessage = fmt.Sprintf("%v %x\n\n%v", markerAnchor, *anchorKey,
		commitMessage)

	// Commit merkle root as an anchor and append included commits to audit
	// trail
	err = g.appendAuditTrail(path, anchorRecord.Time, *anchorKey,
		auditLines)
	if err != nil {
		return nil, fmt.Errorf("could not append to audit trail: %v",
			err)
	}
	err = g.gitAdd(path, defaultAuditTrailFile)
	if err != nil {
		return nil, fmt.Errorf("gitAdd: %v", err)
	}
	err = g.gitCommit(path, commitMessage)
	if err != nil {
		return nil, fmt.Errorf("gitCommit: %v", err)
	}

	return anchorKey, nil
}

// anchor verifies if there are new commits in all repos and if that is the
// case it drops and anchor in dcrtime for each of them.
func (g *gitBackEnd) anchorAllRepos() error {
	log.Infof("Dropping anchor")
	// Lock filesystem
	err := g.lock.Lock(LockDuration)
	if err != nil {
		return fmt.Errorf("anchorAllRepos lock error: %v", err)
	}
	defer func() {
		err := g.lock.Unlock()
		if err != nil {
			log.Errorf("anchorAllRepos unlock error: %v", err)
		}
	}()
	if g.shutdown {
		return fmt.Errorf("anchorAllRepos: %v", backend.ErrShutdown)
	}

	//  Anchor vetted
	log.Infof("Anchoring %v", g.vetted)
	mr, err := g.anchorRepo(g.vetted)
	if err != nil {
		if err == errNothingToDo {
			log.Infof("Anchoring %v: nothing to do", g.vetted)
			return nil
		}
		return fmt.Errorf("anchor repo %v: %v", g.vetted, err)
	}

	// Sync vetted to unvetted

	// git pull --ff-only --rebase
	err = g.gitPull(g.unvetted, true)
	if err != nil {
		return err
	}

	log.Infof("Dropping anchor complete: %x", *mr)

	return nil
}

// periodicAnchorChecker must be run as a go routine.  It sits around and
// periodically checks if there is work to do.  It can also be tickled by
// messaging checkAnchor.
func (g *gitBackEnd) periodicAnchorChecker() {
	log.Infof("Periodic anchor checker launched")
	defer log.Infof("Periodic anchor checker exited")
	for {
		select {
		case <-g.exit:
			return
		case <-g.checkAnchor:
		case <-time.After(5 * time.Minute):
		}

		if g.shutdown {
			return
		}

		// Do lengthy work, this may have to be its own go routine
		err := g.anchorChecker()
		if err != nil {
			// Not much we can do past logging
			log.Errorf("periodicAnchorChecker: %v", err)
		}
	}
}

// anchorChecker does the work for periodicAnchorChecker.  It lives in its own
// function for testing purposes.
func (g *gitBackEnd) anchorChecker() error {
	ua, err := g.readUnconfirmedAnchorRecord()
	if err != nil {
		return fmt.Errorf("anchorChecker read: %v", err)
	}

	// Check for work
	if len(ua.Merkles) == 0 {
		return nil
	}

	// Do one verify at a time for now
	vrs := make([]v1.VerifyDigest, 0, len(ua.Merkles))
	for _, u := range ua.Merkles {
		digest := hex.EncodeToString(u)
		vr, err := g.verifyAnchor(digest)
		if err != nil {
			log.Errorf("anchorChecker verify: %v", err)
			continue
		}
		vrs = append(vrs, *vr)
	}

	err = g.afterAnchorVerify(vrs)
	if err != nil {
		return fmt.Errorf("afterAnchorVerify: %v", err)
	}

	return nil
}

// afterAnchorVerify completes the anchor verification process.  It is a
// separate function in order not having to futz with locks.
func (g *gitBackEnd) afterAnchorVerify(vrs []v1.VerifyDigest) error {
	// Lock filesystem
	err := g.lock.Lock(LockDuration)
	if err != nil {
		return err
	}
	defer func() {
		err := g.lock.Unlock()
		if err != nil {
			log.Errorf("afterAnchorVerify unlock error: %v", err)
		}
	}()

	if len(vrs) != 0 {
		// git checkout master
		err = g.gitCheckout(g.vetted, "master")
		if err != nil {
			return err
		}
	}
	// Handle verified vrs
	for _, vr := range vrs {
		if vr.ChainInformation.ChainTimestamp == 0 {
			// dcrtime returns 0 when there are not enough
			// confirmations yet.
			return fmt.Errorf("not enough confirmations: %v",
				vr.Digest)
		}

		// Use the audit trail as the file to be committed
		mr, ok := util.ConvertDigest(vr.Digest)
		if !ok {
			return fmt.Errorf("invalid digest: %v", vr.Digest)
		}
		txLine := fmt.Sprintf("%v anchored in TX %v\n", vr.Digest,
			vr.ChainInformation.Transaction)
		err = g.appendAuditTrail(g.vetted,
			vr.ChainInformation.ChainTimestamp, mr, []string{txLine})
		if err != nil {
			return err
		}
		err = g.gitAdd(g.vetted, defaultAuditTrailFile)
		if err != nil {
			return err
		}

		// Store dcrtime information.
		// In vetted store the ChainInformation as a json object in
		// directory anchor.
		// In Vetted in the record directory add a file called anchor
		// that points to the TX id.
		anchorDir := filepath.Join(g.vetted, defaultAnchorsDirectory)
		err = os.MkdirAll(anchorDir, 0774)
		if err != nil {
			return err
		}
		ar, err := json.Marshal(vr.ChainInformation)
		if err != nil {
			return err
		}
		err = ioutil.WriteFile(filepath.Join(anchorDir, vr.Digest),
			ar, 0664)
		if err != nil {
			return err
		}
		err = g.gitAdd(g.vetted,
			filepath.Join(defaultAnchorsDirectory, vr.Digest))
		if err != nil {
			return err
		}

		// git commit anchor confirmation
		commitMsg := markerAnchorConfirmation + " " + vr.Digest + "\n\n" + txLine
		err = g.gitCommit(g.vetted, commitMsg)
		if err != nil {
			return err
		}

		// Mark test anchors as confirmed by dcrtime
		if g.test {
			g.testAnchors[vr.Digest] = true
		}
	}
	if len(vrs) != 0 {
		// git checkout master unvetted
		err = g.gitCheckout(g.unvetted, "master")
		if err != nil {
			return err
		}

		// git pull --ff-only --rebase
		err = g.gitPull(g.unvetted, true)
		if err != nil {
			return err
		}
	}

	return nil
}

// anchorAllReposCronJob is the cron job that anchors all repos at a preset time.
func (g *gitBackEnd) anchorAllReposCronJob() {
	err := g.anchorAllRepos()
	if err != nil {
		log.Errorf("%v", err)
	}
}

// verifyAnchor asks dcrtime if an anchor has been verified and returns a TX if
// it has.
func (g *gitBackEnd) verifyAnchor(digest string) (*v1.VerifyDigest, error) {
	var (
		vr  *v1.VerifyReply
		err error
	)

	// In test mode we fake success.
	if g.test {
		// Fake success
		vr = &v1.VerifyReply{}
		anchored, ok := g.testAnchors[digest]
		if !ok {
			return nil, fmt.Errorf("test not found")
		}
		if anchored {
			return nil, fmt.Errorf("already anchored")
		}
		vr.Digests = append(vr.Digests, v1.VerifyDigest{
			Digest: digest,
			Result: v1.ResultOK,
			ChainInformation: v1.ChainInformation{
				ChainTimestamp: time.Now().Unix(),
				Transaction:    expectedTestTX,
			},
		})
	} else {
		// Call dcrtime
		vr, err = util.Verify(g.dcrtimeHost, []string{digest})
		if err != nil {
			return nil, err
		}
	}

	// Do some sanity checks
	if len(vr.Digests) != 1 {
		return nil, fmt.Errorf("unexpected number of digests")
	}
	if vr.Digests[0].Result != v1.ResultOK {
		return nil, fmt.Errorf("unexpected result: %v",
			vr.Digests[0].Result)
	}

	return &vr.Digests[0], nil
}

// newRecord adds a new record to the unvetted repo.  Note that this function
// must be wrapped by a function that delivers the call with the unvetted repo
// sitting in master.  The idea is that if this function fails we can simply
// unwind it by calling a git stash.
// Function must be called with the lock held.
func (g *gitBackEnd) newRecord(token []byte, metadata []backend.MetadataStream, fa []file) (*backend.RecordMetadata, error) {
	id := hex.EncodeToString(token)

	// git checkout -b id
	err := g.gitNewBranch(g.unvetted, id)
	if err != nil {
		return nil, err
	}

	// Process files.
	path := filepath.Join(g.unvetted, id, defaultPayloadDir)
	err = os.MkdirAll(path, 0774)
	if err != nil {
		return nil, err
	}

	hashes := make([]*[sha256.Size]byte, 0, len(fa))
	for i := range fa {
		// Copy files into directory id/payload/filename.
		filename := filepath.Join(path, fa[i].name)
		err = ioutil.WriteFile(filename, fa[i].payload, 0664)
		if err != nil {
			return nil, err
		}
		var d [sha256.Size]byte
		copy(d[:], fa[i].digest)
		hashes = append(hashes, &d)

		// git add id/payload/filename
		err = g.gitAdd(g.unvetted, filename)
		if err != nil {
			return nil, err
		}

	}

	// Save all metadata streams
	for i := range metadata {
		filename := filepath.Join(g.unvetted, id, fmt.Sprintf("%02v%v",
			metadata[i].ID, defaultMDFilenameSuffix))
		err = ioutil.WriteFile(filename, []byte(metadata[i].Payload),
			0664)
		if err != nil {
			return nil, err
		}
		// git add id/metadata.txt
		err = g.gitAdd(g.unvetted, filename)
		if err != nil {
			return nil, err
		}
	}

	// Save record metadata
	brm, err := createMD(g.unvetted, id, backend.MDStatusUnvetted, 1,
		hashes, token)
	if err != nil {
		return nil, err
	}

	// git add id/recordmetadata.json
	filename := filepath.Join(g.unvetted, id, defaultRecordMetadataFilename)
	err = g.gitAdd(g.unvetted, filename)
	if err != nil {
		return nil, err
	}

	// git commit -m "message"
	err = g.gitCommit(path, "Add record "+id)
	if err != nil {
		return nil, err
	}

	return brm, nil
}

// New takes a record verifies it and drops it on disk in the unvetted
// directory.  Records and metadata are stored in unvetted/token/.  the
// function returns a RecordMetadata.
//
// New satisfies the backend interface.
func (g *gitBackEnd) New(metadata []backend.MetadataStream, files []backend.File) (*backend.RecordMetadata, error) {
	fa, err := verifyContent(metadata, files, []string{})
	if err != nil {
		return nil, err
	}

	// Create a censorship token.
	token, err := util.Random(pd.TokenSize)
	if err != nil {
		return nil, err
	}

	// Lock filesystem
	err = g.lock.Lock(LockDuration)
	if err != nil {
		return nil, err
	}
	defer func() {
		err := g.lock.Unlock()
		if err != nil {
			log.Errorf("Unlock error: %v", err)
		}
	}()
	if g.shutdown {
		return nil, backend.ErrShutdown
	}

	// git checkout master
	err = g.gitCheckout(g.unvetted, "master")
	if err != nil {
		return nil, err
	}

	// git pull --ff-only --rebase
	err = g.gitPull(g.unvetted, true)
	if err != nil {
		return nil, err
	}

	var errReturn error
	brm, err := g.newRecord(token, metadata, fa)
	if err != nil {
		// git stash
		err2 := g.gitStash(g.unvetted)
		if err2 != nil {
			// We are in trouble!  Consider a panic.
			log.Errorf("gitStash: %v", err2)
			return nil, err2
		}

		brm = nil
		errReturn = err
	}

	// git checkout master
	err = g.gitCheckout(g.unvetted, "master")
	if err != nil {
		return nil, err
	}

	return brm, errReturn
}

// updateMetadata appends or overwrites in the unvetted repository.
// Additionally it does the git bits when called.
// Function must be called with the lock held.
func (g *gitBackEnd) updateMetadata(id string, mdAppend, mdOverwrite []backend.MetadataStream) error {
	// Overwrite metadata
	for i := range mdOverwrite {
		filename := filepath.Join(g.unvetted, id, fmt.Sprintf("%02v%v",
			mdOverwrite[i].ID, defaultMDFilenameSuffix))
		err := ioutil.WriteFile(filename, []byte(mdOverwrite[i].Payload),
			0664)
		if err != nil {
			return err
		}
		// git add id/metadata.txt
		err = g.gitAdd(g.unvetted, filename)
		if err != nil {
			return err
		}
	}

	// Append metadata
	for i := range mdAppend {
		filename := filepath.Join(g.unvetted, id, fmt.Sprintf("%02v%v",
			mdAppend[i].ID, defaultMDFilenameSuffix))
		f, err := os.OpenFile(filename,
			os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			return err
		}
		_, err = io.WriteString(f, mdAppend[i].Payload)
		if err != nil {
			f.Close()
			return err
		}
		f.Close()
		// git add id/metadata.txt
		err = g.gitAdd(g.unvetted, filename)
		if err != nil {
			return err
		}
	}
	return nil
}

func (g *gitBackEnd) checkoutRecordBranch(id string) (bool, error) {
	// See if branch already exists
	branches, err := g.gitBranches(g.unvetted)
	if err != nil {
		return false, err
	}
	var found bool
	for _, v := range branches {
		if !util.IsDigest(v) {
			continue
		}
		if v == id {
			found = true
			break
		}
	}

	if found {
		// Branch exists, modify branch
		err := g.gitCheckout(g.unvetted, id)
		if err != nil {
			return true, backend.ErrRecordNotFound
		}
	} else {
		// Branch does not exist, create it if record exists
		fi, err := os.Stat(filepath.Join(g.unvetted, id))
		if err != nil {
			if os.IsNotExist(err) {
				return false, backend.ErrRecordNotFound
			}
		}
		if !fi.IsDir() {
			return false, fmt.Errorf("unvetted repo corrupt: %v "+
				"is not a dir", fi.Name())
		}
		// git checkout -b id
		err = g.gitNewBranch(g.unvetted, id)
		if err != nil {
			return false, err
		}
	}

	return found, nil
}

// updateRecord takes various parameters to update a record.  Note that this
// function must be wrapped by a function that delivers the call with the
// unvetted repo sitting in master.  The idea is that if this function fails we
// can simply unwind it by calling a git stash.
// Function must be called with the lock held.
func (g *gitBackEnd) updateRecord(token []byte, mdAppend, mdOverwrite []backend.MetadataStream, fa []file, filesDel []string) (*backend.RecordMetadata, error) {
	// Checkout branch
	id := hex.EncodeToString(token)
	_, err := g.checkoutRecordBranch(id)
	if err != nil {
		return nil, err
	}

	// We now are sitting in branch id

	// Load MD
	log.Tracef("updating %x", token)
	brm, err := loadMD(g.unvetted, id)
	if err != nil {
		return nil, err
	}
	if !(brm.Status == backend.MDStatusVetted ||
		brm.Status == backend.MDStatusUnvetted ||
		brm.Status == backend.MDStatusIterationUnvetted ||
		brm.Status == backend.MDStatusLocked) {
		return nil, fmt.Errorf("can not update record that "+
			"has status: %v %v", brm.Status,
			backend.MDStatus[brm.Status])
	}

	// Verify all deletes before executing
	for _, v := range filesDel {
		fi, err := os.Stat(filepath.Join(g.unvetted, id,
			defaultPayloadDir, v))
		if err != nil {
			if os.IsNotExist(err) {
				return nil, backend.ContentVerificationError{
					ErrorCode:    pd.ErrorStatusFileNotFound,
					ErrorContext: []string{v},
				}
			}
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("not a file: %v", fi.Name())
		}
	}

	// At this point we should be ready to add/remove/update all the things.
	path := filepath.Join(g.unvetted, id, defaultPayloadDir)
	for i := range fa {
		// Copy files into directory id/payload/filename.
		filename := filepath.Join(path, fa[i].name)
		err = ioutil.WriteFile(filename, fa[i].payload, 0664)
		if err != nil {
			return nil, err
		}

		// git add id/payload/filename
		err = g.gitAdd(g.unvetted, filename)
		if err != nil {
			return nil, err
		}
	}

	// Delete files
	for _, v := range filesDel {
		err = g.gitRm(g.unvetted, filepath.Join(id, defaultPayloadDir,
			v))
		if err != nil {
			return nil, err
		}
	}

	// Handle metadata
	err = g.updateMetadata(id, mdAppend, mdOverwrite)
	if err != nil {
		return nil, err
	}

	// Find all hashes
	hashes := make([]*[sha256.Size]byte, 0, len(fa))
	ppath := filepath.Join(g.unvetted, id, defaultPayloadDir)
	newRecordFiles, err := ioutil.ReadDir(ppath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, backend.ContentVerificationError{
				ErrorCode: pd.ErrorStatusEmpty,
			}
		}
		return nil, err
	}
	for _, v := range newRecordFiles {
		digest, err := util.DigestFileBytes(filepath.Join(ppath,
			v.Name()))
		if err != nil {
			return nil, err
		}
		var d [sha256.Size]byte
		copy(d[:], digest)
		hashes = append(hashes, &d)
	}

	// If there are no changes DO NOT update the record and reply with no
	// changes.
	o, err := g.gitDiff(g.unvetted)
	if err != nil {
		return nil, err
	}
	if len(o) == 0 {
		return nil, backend.ErrNoChanges
	}

	// Update record metadata
	brmNew, err := createMD(g.unvetted, id,
		backend.MDStatusIterationUnvetted, brm.Version+1, hashes, token)
	if err != nil {
		return nil, err
	}

	// git add id/recordmetadata.json
	filename := filepath.Join(g.unvetted, id, defaultRecordMetadataFilename)
	err = g.gitAdd(g.unvetted, filename)
	if err != nil {
		return nil, err
	}

	// git commit -m "message"
	err = g.gitCommit(path, "Update record "+id)
	if err != nil {
		return nil, err
	}

	return brmNew, nil
}

func (g *gitBackEnd) UpdateUnvettedRecord(token []byte, mdAppend []backend.MetadataStream, mdOverwrite []backend.MetadataStream, filesAdd []backend.File, filesDel []string) (*backend.RecordMetadata, error) {
	// Send in a single metadata array to verify there are no dups.
	allMD := append(mdAppend, mdOverwrite...)
	fa, err := verifyContent(allMD, filesAdd, filesDel)
	if err != nil {
		e, ok := err.(backend.ContentVerificationError)
		if !ok {
			return nil, err
		}
		// Allow ErrorStatusEmpty
		if e.ErrorCode != pd.ErrorStatusEmpty {
			return nil, err
		}
	}

	// Lock filesystem
	err = g.lock.Lock(LockDuration)
	if err != nil {
		return nil, err
	}
	defer func() {
		err := g.lock.Unlock()
		if err != nil {
			log.Errorf("Unlock error: %v", err)
		}
	}()
	if g.shutdown {
		return nil, backend.ErrShutdown
	}

	// git checkout master
	err = g.gitCheckout(g.unvetted, "master")
	if err != nil {
		return nil, err
	}

	// git pull --ff-only --rebase
	err = g.gitPull(g.unvetted, true)
	if err != nil {
		return nil, err
	}

	log.Tracef("updating %x", token)
	// Do the work, if there is an error we must unwind git.
	var errReturn error
	brm, err := g.updateRecord(token, mdAppend, mdOverwrite, fa, filesDel)
	if err == backend.ErrNoChanges {
		brm = nil
		errReturn = err
	} else if err != nil {
		// git stash
		err2 := g.gitStash(g.unvetted)
		if err2 != nil {
			// We are in trouble! Consider a panic.
			log.Errorf("gitStash: %v", err2)
			return nil, err2
		}

		brm = nil
		errReturn = err
	}

	// git checkout master
	err = g.gitCheckout(g.unvetted, "master")
	if err != nil {
		return nil, err
	}

	return brm, errReturn
}

// updateVettedMetadata updates metadata in the unvetted repo and pushes it
// upstream followed by a rebase.  Record is not updated.
// This function must be called with the lock held.
func (g *gitBackEnd) updateVettedMetadata(id, idTmp string, mdAppend []backend.MetadataStream, mdOverwrite []backend.MetadataStream) error {
	// Checkout temporary branch
	err := g.gitNewBranch(g.unvetted, idTmp)
	if err != nil {
		return err
	}

	// Update metadata changes
	err = g.updateMetadata(id, mdAppend, mdOverwrite)
	if err != nil {
		return err
	}

	// If there are no changes DO NOT update the record and reply with no
	// changes.
	if !g.gitHasChanges(g.unvetted) {
		return backend.ErrNoChanges
	}

	// Commit change
	err = g.gitCommit(g.unvetted, "Update record metadata "+id)
	if err != nil {
		return err
	}

	// create and rebase PR
	return g.rebasePR(idTmp)
}

// UpdateVettedMetadata updates metadata in vetted record.  It goes through the
// normal stages of updating unvetted, pushing PR, merge PR, pull remote.
// Record itself is not changed.
func (g *gitBackEnd) UpdateVettedMetadata(token []byte, mdAppend []backend.MetadataStream, mdOverwrite []backend.MetadataStream) error {
	// Send in a single metadata array to verify there are no dups.
	allMD := append(mdAppend, mdOverwrite...)
	_, err := verifyContent(allMD, []backend.File{}, []string{})
	if err != nil {
		e, ok := err.(backend.ContentVerificationError)
		if !ok {
			return err
		}
		// Allow ErrorStatusEmpty
		if e.ErrorCode != pd.ErrorStatusEmpty {
			return err
		}
	}

	// Lock filesystem
	err = g.lock.Lock(LockDuration)
	if err != nil {
		return err
	}
	defer func() {
		err := g.lock.Unlock()
		if err != nil {
			log.Errorf("Unlock error: %v", err)
		}
	}()
	if g.shutdown {
		return backend.ErrShutdown
	}

	// git checkout master
	err = g.gitCheckout(g.unvetted, "master")
	if err != nil {
		return err
	}

	// git pull --ff-only --rebase
	err = g.gitPull(g.unvetted, true)
	if err != nil {
		return err
	}

	// Check if temporary branch exists (should never be the case)
	id := hex.EncodeToString(token)
	idTmp := id + "_tmp"

	// Make sure vetted exists
	_, err = os.Stat(filepath.Join(g.unvetted, id))
	if err != nil {
		if os.IsNotExist(err) {
			return backend.ErrRecordNotFound
		}
	}

	// Make sure record is not locked.
	md, err := loadMD(g.unvetted, id)
	if err != nil {
		return err
	}
	if md.Status == backend.MDStatusLocked {
		return backend.ErrRecordLocked
	}

	log.Tracef("updating vetted metadata %x", token)

	// Do the work, if there is an error we must unwind git.
	var errReturn error
	err = g.updateVettedMetadata(id, idTmp, mdAppend, mdOverwrite)
	if err != nil {
		// git stash and drop potential tmp branch
		err2 := g.gitStash(g.unvetted)
		if err2 != nil {
			// We are in trouble! Consider a panic.
			log.Errorf("gitStash: %v", err2)
			return err2
		}

		errReturn = err
	}

	// git checkout master
	err = g.gitCheckout(g.unvetted, "master")
	if err != nil {
		return err
	}

	// If something went wrong drop branch
	if errReturn != nil {
		err2 := g.gitBranchDelete(g.unvetted, idTmp)
		if err2 != nil {
			// We are in trouble! Consider a panic.
			log.Errorf("gitBranchDelete: %v", err2)
			return err2
		}
	}

	return errReturn
}

// getRecordLock is the generic implementation of GetUnvetted/GetVetted.  It
// returns a record record from the provided repo.
//
// This function must be called WITHOUT the lock held.
func (g *gitBackEnd) getRecordLock(token []byte, repo string, includeFiles bool) (*backend.Record, error) {
	// Lock filesystem
	err := g.lock.Lock(LockDuration)
	if err != nil {
		return nil, err
	}
	defer func() {
		err := g.lock.Unlock()
		if err != nil {
			log.Errorf("Unlock error: %v", err)
		}
	}()
	if g.shutdown {
		return nil, backend.ErrShutdown
	}

	return g.getRecord(token, repo, includeFiles)
}

// _getRecord loads a record from the current branch on the provided repo.
//
// This function must be called WITH the lock held.
func (g *gitBackEnd) _getRecord(id, repo string, includeFiles bool) (*backend.Record, error) {
	// load MD
	brm, err := loadMD(repo, id)
	if err != nil {
		return nil, err
	}

	// load metadata streams
	mds, err := loadMDStreams(repo, id)
	if err != nil {
		return nil, err
	}

	var files []backend.File
	if includeFiles {
		// load files
		files, err = loadRecord(repo, id)
		if err != nil {
			return nil, err
		}
	}

	return &backend.Record{
		RecordMetadata: *brm,
		Metadata:       mds,
		Files:          files,
	}, nil
}

// getRecord is the generic implementation of GetUnvetted/GetVetted.  It
// returns a record record from the provided repo.
//
// This function must be called WITH the lock held.
func (g *gitBackEnd) getRecord(token []byte, repo string, includeFiles bool) (*backend.Record, error) {
	id := hex.EncodeToString(token)
	if repo == g.unvetted {
		// git checkout id
		err := g.gitCheckout(repo, id)
		if err != nil {
			return nil, backend.ErrRecordNotFound
		}
		branchNow, err := g.gitBranchNow(repo)
		if err != nil || branchNow != id {
			return nil, backend.ErrRecordNotFound
		}
	}
	defer func() {
		// git checkout master
		err := g.gitCheckout(repo, "master")
		if err != nil {
			log.Errorf("could not switch to master: %v", err)
		}
	}()

	return g._getRecord(id, repo, includeFiles)
}

// fsck performs a git fsck and additionally it validates the git tree against
// dcrtime.  This is an expensive operation and should not be run during
// runtime.
//
// This function must be called WITH holding the lock.
func (g *gitBackEnd) fsck(path string) error {
	// obtain all commit digests and verify them.  We don't store anchor
	// confirmations so we have to skip those.
	out, err := g.git(path, "log", "--pretty=oneline")
	if err != nil {
		return err
	}
	if len(out) == 0 {
		return fmt.Errorf("invalid git output")
	}

	var seenAnchor bool
	// gitDigests is an index of all git digests to verify with dcrtime
	gitDigests := make(map[string]struct{})
	// confirmedAnchors keeps track of anchors that were timestamped with dcrtime but not verified,
	// since periodicAnchorChecker only checks recent unconfirmed anchors and ignores older ones
	confirmedAnchors := make(map[string]struct{})
	var unconfirmedAnchors []string
	for _, v := range out {
		if regexAnchorConfirmation.MatchString(v) {
			// Store confirmed anchor merkle roots to look up later
			merkleRoot := regexAnchorConfirmation.FindStringSubmatch(v)[1]
			confirmedAnchors[merkleRoot] = struct{}{}
			continue
		} else if regexAnchor.MatchString(v) {
			// We now have seen an Anchor commit. The following digests are now precious.
			seenAnchor = true
			// We should have seen its confirmation already, since we're parsing top to bottom
			// If we didn't, save the anchor key to verify with dcrtime later
			merkleRoot := regexAnchor.FindStringSubmatch(v)[1]
			_, confirmed := confirmedAnchors[merkleRoot]
			if !confirmed {
				unconfirmedAnchors = append(unconfirmedAnchors, merkleRoot)
			}
			continue
		}
		if !seenAnchor {
			// We have not seen an Anchor yet so this digest is not
			// precious.
			continue
		}
		// git output is digest followed by one liner commit message
		s := strings.SplitN(v, " ", 2)
		if len(s) != 2 {
			log.Infof("%v", spew.Sdump(s))
			return fmt.Errorf("unexpected split: %v", v)
		}
		ds, err := extendSHA1FromString(s[0])
		if err != nil {
			return fmt.Errorf("not a digest: %v", v)
		}
		if _, ok := gitDigests[ds]; ok {
			return fmt.Errorf("duplicate git digest: %v", ds)
		}
		gitDigests[ds] = struct{}{}
	}

	if len(gitDigests) == 0 {
		log.Infof("fsck: nothing to do")
		return nil
	}

	log.Infof("fsck: dcrtime verification started")

	// Verify the unconfirmed anchors
	vrs := make([]v1.VerifyDigest, 0, len(unconfirmedAnchors))
	for _, merkleRoot := range unconfirmedAnchors {
		vr, err := g.verifyAnchor(merkleRoot)
		if err != nil {
			log.Errorf("Error verifying anchor during fsck: %v", err)
			continue
		} else {
			vrs = append(vrs, *vr)
		}
	}

	err = g.afterAnchorVerify(vrs)
	if err != nil {
		return err
	}

	// Now we should be able to verify all the precious git digests
	digests := make([]string, 0, len(gitDigests))
	for d := range gitDigests {
		digests = append(digests, d)
	}
	vr, err := util.Verify(g.dcrtimeHost, digests)
	if err != nil {
		return err
	}

	// Verify all results
	var fail bool
	for _, v := range vr.Digests {
		if v.Result != v1.ResultOK {
			fail = true
			log.Errorf("dcrtime error: %v %v %v", v.Digest,
				v.Result, v1.Result[v.Result])
		}
	}
	if fail {
		return fmt.Errorf("dcrtime fsck failed")
	}

	return nil
}

// GetUnvetted checks out branch token and returns the content of
// unvetted/token directory.
//
// GetUnvetted satisfies the backend interface.
func (g *gitBackEnd) GetUnvetted(token []byte) (*backend.Record, error) {
	return g.getRecordLock(token, g.unvetted, true)
}

// GetVetted returns the content of vetted/token directory.
//
// GetVetted satisfies the backend interface.
func (g *gitBackEnd) GetVetted(token []byte) (*backend.Record, error) {
	return g.getRecordLock(token, g.vetted, true)
}

// setUnvettedStatus takes various parameters to update a record metadata and
// status.  Note that this function must be wrapped by a function that delivers
// the call with the unvetted repo sitting in master.  The idea is that if this
// function fails we can simply unwind it by calling a git stash.
// Function must be called with the lock held.
func (g *gitBackEnd) setUnvettedStatus(token []byte, status backend.MDStatusT, mdAppend, mdOverwrite []backend.MetadataStream) (*backend.Record, error) {
	// git checkout id
	id := hex.EncodeToString(token)
	err := g.gitCheckout(g.unvetted, id)
	if err != nil {
		return nil, backend.ErrRecordNotFound
	}

	// Load record
	record, err := g._getRecord(id, g.unvetted, false)
	if err != nil {
		return nil, err
	}

	// We only allow a transition from unvetted to vetted or censored
	switch {
	case (record.RecordMetadata.Status == backend.MDStatusUnvetted ||
		record.RecordMetadata.Status == backend.MDStatusIterationUnvetted) &&
		status == backend.MDStatusVetted:

		// unvetted -> vetted

		// Update MD first
		record.RecordMetadata.Status = backend.MDStatusVetted
		record.RecordMetadata.Version += 1
		record.RecordMetadata.Timestamp = time.Now().Unix()
		err = updateMD(g.unvetted, id, &record.RecordMetadata)
		if err != nil {
			return nil, err
		}

		// Handle metadata
		err = g.updateMetadata(id, mdAppend, mdOverwrite)
		if err != nil {
			return nil, err
		}

		// Commit brm
		err = g.commitMD(g.unvetted, id, "published")
		if err != nil {
			return nil, err
		}

		// Create and rebase PR
		err = g.rebasePR(id)
		if err != nil {
			return nil, err
		}

	case record.RecordMetadata.Status == backend.MDStatusUnvetted &&
		status == backend.MDStatusCensored:
		// unvetted -> censored
		record.RecordMetadata.Status = backend.MDStatusCensored
		record.RecordMetadata.Version += 1
		record.RecordMetadata.Timestamp = time.Now().Unix()
		err = updateMD(g.unvetted, id, &record.RecordMetadata)
		if err != nil {
			return nil, err
		}

		// Handle metadata
		err = g.updateMetadata(id, mdAppend, mdOverwrite)
		if err != nil {
			return nil, err
		}

		// Commit brm
		err = g.commitMD(g.unvetted, id, "censored")
		if err != nil {
			return nil, err
		}
	default:
		return nil, backend.StateTransitionError{
			From: record.RecordMetadata.Status,
			To:   status,
		}
	}

	return record, nil
}

// SetUnvettedStatus tries to update the status for an unvetted record. It
// returns the updated record if successful but without the Files compnonet.
//
// SetUnvettedStatus satisfies the backend interface.
func (g *gitBackEnd) SetUnvettedStatus(token []byte, status backend.MDStatusT, mdAppend, mdOverwrite []backend.MetadataStream) (*backend.Record, error) {
	// Lock filesystem
	err := g.lock.Lock(LockDuration)
	if err != nil {
		return nil, err
	}
	defer func() {
		err := g.lock.Unlock()
		if err != nil {
			log.Errorf("Unlock error: %v", err)
		}
	}()
	if g.shutdown {
		return nil, backend.ErrShutdown
	}

	log.Tracef("setting status %v (%v) -> %x", status,
		backend.MDStatus[status], token)
	var errReturn error
	record, err := g.setUnvettedStatus(token, status, mdAppend, mdOverwrite)
	if err != nil {
		// git stash
		err2 := g.gitStash(g.unvetted)
		if err2 != nil {
			// We are in trouble!  Consider a panic.
			log.Errorf("gitStash: %v", err2)
			return nil, err2
		}
		errReturn = err
	}

	// git checkout master
	err = g.gitCheckout(g.unvetted, "master")
	if err != nil {
		return nil, err
	}

	if errReturn != nil {
		return nil, errReturn
	}

	return record, nil
}

// Inventory returns an inventory of vetted and unvetted records.  If
// includeFiles is set the content is also returned.
func (g *gitBackEnd) Inventory(vettedCount, branchCount uint, includeFiles bool) ([]backend.Record, []backend.Record, error) {
	// Lock filesystem
	err := g.lock.Lock(LockDuration)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		err := g.lock.Unlock()
		if err != nil {
			log.Errorf("Unlock error: %v", err)
		}
	}()
	if g.shutdown {
		return nil, nil, backend.ErrShutdown
	}

	// Walk vetted, we can simply take the vetted directory and sort the
	// entries by time.
	files, err := ioutil.ReadDir(g.vetted)
	if err != nil {
		return nil, nil, err
	}

	// Strip non record directories
	pr := make([]backend.Record, 0, len(files))
	for _, v := range files {
		id := v.Name()
		if !util.IsDigest(id) {
			continue
		}

		ids, err := hex.DecodeString(id)
		if err != nil {
			return nil, nil, err
		}
		prv, err := g.getRecord(ids, g.vetted, includeFiles)
		if err != nil {
			return nil, nil, err
		}
		pr = append(pr, *prv)
	}

	// Walk Branches on unvetted
	branches, err := g.gitBranches(g.unvetted)
	if err != nil {
		return nil, nil, err
	}
	br := make([]backend.Record, 0, len(branches))
	for _, id := range branches {
		if !util.IsDigest(id) {
			continue
		}

		ids, err := hex.DecodeString(id)
		if err != nil {
			return nil, nil, err
		}
		pru, err := g.getRecord(ids, g.unvetted, includeFiles)
		if err != nil {
			return nil, nil, err
		}
		br = append(br, *pru)
	}

	return pr, br, nil
}

// GetPlugins returns a list of currently supported plugins and their settings.
//
// GetPlugins satisfies the backend interface.
func (g *gitBackEnd) GetPlugins() ([]backend.Plugin, error) {
	return g.plugins, nil
}

// Plugin send a passthrough command. The return values are: incomming command
// identifier, encoded command result and an error if the command failed to
// execute.
//
// Plugin satisfies the backend interface.
func (g *gitBackEnd) Plugin(command, payload string) (string, string, error) {
	log.Tracef("Plugin: %v %v", command, payload)
	switch command {
	case decredplugin.CmdStartVote:
		payload, err := g.pluginStartVote(payload)
		return decredplugin.CmdStartVote, payload, err
	case decredplugin.CmdCastVotes:
		payload, err := g.pluginCastVotes(payload)
		return decredplugin.CmdCastVotes, payload, err
	case decredplugin.CmdBestBlock:
		payload, err := g.pluginBestBlock()
		return decredplugin.CmdBestBlock, payload, err
	}
	return "", "", fmt.Errorf("invalid payload command") // XXX this needs to become a type error
}

// Close shuts down the backend.  It obtains the lock and sets the shutdown
// boolean to true.  All interface functions MUST return with errShutdown if
// the backend is shutting down.
//
// Close satisfies the backend interface.
func (g *gitBackEnd) Close() {
	err := g.lock.Lock(LockDuration)
	if err != nil {
		log.Errorf("Lock error: %v", err)
		return
	}
	defer func() {
		err := g.lock.Unlock()
		if err != nil {
			log.Errorf("Unlock error: %v", err)
		}
	}()

	g.shutdown = true
	close(g.exit)
}

// newLocked runs the portion of new that has to be locked.
func (g *gitBackEnd) newLocked() error {
	// Initialize global filesystem lock
	var err error
	g.lock, err = lockfile.New(filepath.Join(g.root,
		LockFilename), 100*time.Millisecond)
	if err != nil {
		return err
	}
	err = g.lock.Lock(LockDuration)
	if err != nil {
		return err
	}
	defer func() {
		err := g.lock.Unlock()
		if err != nil {
			log.Errorf("New unlock error: %v", err)
		}
	}()

	// Ensure git works
	version, err := g.gitVersion()
	if err != nil {
		return err
	}

	log.Infof("Git version: %v", version)

	// Init vetted git repo
	err = g.gitInitRepo(g.vetted, defaultRepoConfig)
	if err != nil {
		return err
	}

	// Clone vetted repo into unvetted
	err = g.gitClone(g.vetted, g.unvetted, defaultRepoConfig)
	if err != nil {
		return err
	}

	// Fsck _o/
	log.Infof("Running git fsck on vetted repository")
	_, err = g.gitFsck(g.vetted)
	if err != nil {
		return err
	}
	log.Infof("Running git fsck on unvetted repository")
	_, err = g.gitFsck(g.unvetted)
	return err
}

// rebasePR pushes branch id into upstream (vetted repo) and rebases it onto
// master followed by replaying the rebase into origin (unvetted repo).
// This function must be called with the lock held.
func (g *gitBackEnd) rebasePR(id string) error {
	// on unvetted repo:
	//     git checkout master
	//     git pull --ff--only --rebase
	//     git checkout id
	//     git rebase master
	//     git push --set-upstream origin id
	// on vetted repo:
	//     git rebase id
	//     git branch -D id
	// on unvetted repo:
	//     git checkout master
	//     git branch -D id
	//     git pull --ff-only

	//
	// UNVETTED REPO CREATE PR
	//
	// git checkout master
	err := g.gitCheckout(g.unvetted, "master")
	if err != nil {
		return err
	}

	// git pull --ff-only --rebase
	err = g.gitPull(g.unvetted, true)
	if err != nil {
		return err
	}

	// git checkout id
	err = g.gitCheckout(g.unvetted, id)
	if err != nil {
		return backend.ErrRecordNotFound
	}

	// git rebase master
	err = g.gitRebase(g.unvetted, "master")
	if err != nil {
		return err
	}

	// git push --set-upstream origin id
	err = g.gitPush(g.unvetted, "origin", id, true)
	if err != nil {
		return err
	}

	//
	// VETTED REPO REPLAY BRANCH
	//

	// git rebase id
	err = g.gitRebase(g.vetted, id)
	if err != nil {
		return err
	}

	// git branch -D id
	err = g.gitBranchDelete(g.vetted, id)
	if err != nil {
		return err
	}

	//
	// UNVETTED REPO SYNC
	//

	// git checkout master
	err = g.gitCheckout(g.unvetted, "master")
	if err != nil {
		return err
	}

	// git pull --ff-only --rebase
	err = g.gitPull(g.unvetted, true)
	if err != nil {
		return err
	}

	// git branch -D id
	return g.gitBranchDelete(g.unvetted, id)
}

// New returns a gitBackEnd context.  It verifies that git is installed.
func New(anp *chaincfg.Params, root string, dcrtimeHost string, gitPath string, id *identity.FullIdentity, gitTrace bool) (*gitBackEnd, error) {
	// Default to system git
	if gitPath == "" {
		gitPath = "git"
	}

	g := &gitBackEnd{
		activeNetParams: anp,
		root:            root,
		cron:            cron.New(),
		unvetted:        filepath.Join(root, defaultUnvettedPath),
		vetted:          filepath.Join(root, defaultVettedPath),
		gitPath:         gitPath,
		dcrtimeHost:     dcrtimeHost,
		gitTrace:        gitTrace,
		exit:            make(chan struct{}),
		checkAnchor:     make(chan struct{}),
		testAnchors:     make(map[string]bool),
		plugins:         []backend.Plugin{getDecredPlugin(anp.Name != "mainnet")},
	}
	idJSON, err := id.Marshal()
	if err != nil {
		return nil, err
	}
	setDecredPluginSetting(decredPluginIdentity, string(idJSON))

	err = g.newLocked()
	if err != nil {
		return nil, err
	}

	// Launch anchor checker and don't do any work just yet.  The
	// unanchored bits will be picked up during the next go-round.  We
	// don't try to be clever in order to prevent dual commits for the same
	// anchor which can happen if the daemon is launched right around the
	// scheduled anchor drop.
	go g.periodicAnchorChecker()

	// Launch cron.
	err = g.cron.AddFunc(anchorSchedule, func() {
		g.anchorAllReposCronJob()
	})
	if err != nil {
		return nil, err
	}
	g.cron.Start()

	// Message user
	log.Infof("Timestamp host: %v", g.dcrtimeHost)

	log.Infof("Running dcrtime fsck on vetted repository")
	err = g.fsck(g.vetted)
	if err != nil {
		// Log error but continue
		log.Errorf("fsck: dcrtime %v", err)
	}

	return g, nil
}
