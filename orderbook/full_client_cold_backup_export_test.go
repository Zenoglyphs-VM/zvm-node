//go:build fullclient_recovery

package orderbook

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const fullClientColdByteLimit int64 = 256 << 20
const fullClientColdFileLimit = 4096

var fullClientColdConfigs = []string{"apps.json", "full-client-genesis.json", "jwt.hex", "rollup.json"}

type fullClientColdFile struct {
	Name   string
	Bytes  int64
	SHA256 [32]byte
}

// This test-only snapshot contains a closed owned engine, SQLite and their
// declared configuration. It does not implement a live operator backup tool.
type FullClientColdBackup struct {
	directory, source string
	files             []fullClientColdFile
	seal, inputs      [32]byte
	bytes             int64
	sourceRoot        os.FileInfo
	backupRoot        os.FileInfo
	sourceEngine      os.FileInfo
	engineFiles       int
	sqlFiles          int
}

func fullClientColdSeal(files []fullClientColdFile, inputs [32]byte) [32]byte {
	raw, err := json.Marshal(struct {
		Files  []fullClientColdFile
		Inputs [32]byte
	}{files, inputs})
	if err != nil {
		panic("static cold snapshot metadata did not encode")
	}
	return sha256.Sum256(raw)
}

// Open/hash/copy is bounded and checks the same regular file before and after
// reading. Owned handles are closed first; hostile concurrent mutation is not
// qualified by this fixture. No hard link, symlink or recursive archive is used.
func fullClientColdRead(path string, output io.Writer, limit int64) ([32]byte, os.FileInfo, error) {
	var zero [32]byte
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > limit {
		return zero, nil, errors.New("cold snapshot source is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return zero, nil, errors.New("cold snapshot source open failed")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return zero, nil, errors.New("cold snapshot source identity changed before reading")
	}
	hash := sha256.New()
	written, err := io.CopyBuffer(io.MultiWriter(output, hash), io.LimitReader(file, limit+1), make([]byte, 64<<10))
	after, statErr := file.Stat()
	closed := file.Close()
	current, currentErr := os.Lstat(path)
	if err != nil || statErr != nil || closed != nil || currentErr != nil || written != before.Size() ||
		!os.SameFile(before, after) || !os.SameFile(before, current) || !current.Mode().IsRegular() ||
		after.Size() != before.Size() || current.Size() != before.Size() ||
		!after.ModTime().Equal(before.ModTime()) || !current.ModTime().Equal(before.ModTime()) {
		return zero, nil, errors.New("cold snapshot read was incomplete or its source changed")
	}
	copy(zero[:], hash.Sum(nil))
	return zero, before, nil
}

func fullClientColdCopy(source, destination string, limit int64) (fullClientColdFile, error) {
	var result fullClientColdFile
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return result, errors.New("cold snapshot destination directory creation failed")
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return result, errors.New("cold snapshot refuses an existing destination file")
	}
	defer file.Close()
	digest, original, err := fullClientColdRead(source, file, limit)
	if err != nil {
		return result, err
	}
	if err := file.Sync(); err != nil {
		return result, errors.New("cold snapshot destination sync failed")
	}
	if err := file.Close(); err != nil {
		return result, errors.New("cold snapshot destination close failed")
	}
	copied, err := os.Lstat(destination)
	if err != nil || !copied.Mode().IsRegular() || copied.Size() != original.Size() || os.SameFile(original, copied) {
		return result, errors.New("cold snapshot destination is incomplete or aliases its source")
	}
	result.Bytes, result.SHA256 = original.Size(), digest
	return result, nil
}

func fullClientColdNames(directory string) ([]string, error) {
	root, err := os.Lstat(directory)
	if err != nil || !root.IsDir() || root.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("cold snapshot root is not an owned directory")
	}
	names := append([]string(nil), fullClientColdConfigs...)
	names = append(names, "node.db")
	for _, name := range []string{"node.db-wal", "node.db-shm", "node.db-journal"} {
		if _, err := os.Lstat(filepath.Join(directory, name)); err == nil {
			names = append(names, name)
		} else if !os.IsNotExist(err) {
			return nil, errors.New("cold snapshot SQLite companion inventory failed")
		}
	}
	engine := filepath.Join(directory, "full-client")
	if info, err := os.Lstat(engine); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("cold snapshot requires its original full-client directory")
	}
	err = filepath.WalkDir(engine, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("cold snapshot refuses linked or unreadable engine entries")
		}
		info, err := entry.Info()
		if err != nil || !info.IsDir() && !info.Mode().IsRegular() {
			return errors.New("cold snapshot refuses a nonregular engine entry")
		}
		if info.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
			len(relative) > 1024 || strings.Count(filepath.ToSlash(relative), "/") > 32 || len(names) >= fullClientColdFileLimit {
			return errors.New("cold snapshot file inventory exceeded its declared bound")
		}
		names = append(names, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

func (backup *FullClientColdBackup) verify(directory string) error {
	if backup.bytes <= 0 || backup.bytes > fullClientColdByteLimit || len(backup.files) > fullClientColdFileLimit ||
		fullClientColdSeal(backup.files, backup.inputs) != backup.seal {
		return errors.New("cold snapshot manifest differs from its captured seal")
	}
	root, err := os.Lstat(directory)
	if err != nil || !root.IsDir() || root.Mode()&os.ModeSymlink != 0 ||
		directory == backup.source && !os.SameFile(root, backup.sourceRoot) ||
		directory == backup.directory && !os.SameFile(root, backup.backupRoot) {
		return errors.New("cold snapshot root identity changed")
	}
	if directory == backup.source {
		engine, err := os.Lstat(filepath.Join(directory, "full-client"))
		if err != nil || !engine.IsDir() || engine.Mode()&os.ModeSymlink != 0 || !os.SameFile(engine, backup.sourceEngine) {
			return errors.New("cold snapshot original engine directory identity changed")
		}
	}
	names, err := fullClientColdNames(directory)
	if err != nil || len(names) != len(backup.files) {
		return errors.New("cold snapshot file inventory changed")
	}
	var total int64
	for index, name := range names {
		expected := backup.files[index]
		if name != expected.Name || expected.Bytes < 0 || expected.Bytes > fullClientColdByteLimit-total {
			return errors.New("cold snapshot selected files or byte budget changed")
		}
		digest, info, err := fullClientColdRead(filepath.Join(directory, filepath.FromSlash(name)), io.Discard, fullClientColdByteLimit-total)
		if err != nil || info.Size() != expected.Bytes || digest != expected.SHA256 {
			return errors.New("cold snapshot retained bytes do not match the complete manifest")
		}
		total += info.Size()
	}
	if total != backup.bytes {
		return errors.New("cold snapshot aggregate byte count changed")
	}
	return nil
}

// The caller has joined both exact CLI children and removed only its declared
// exit hook. Closing the mode=ro observer does not repair canonical state.
func (f *FullClientSignedRecoveryFixture) ColdPersistenceBackup() *FullClientColdBackup {
	t := f.r.t
	t.Helper()
	if !f.admitted || f.client != nil || f.r.eth != nil || f.r.backend != nil || f.read == nil {
		t.Fatal("cold backup requires admitted inputs and closed parent engine handles")
	}
	if err := f.read.Close(); err != nil {
		t.Fatal("cold backup observer close failed")
	}
	f.read = nil
	f.CheckExecutionClientDirectory()
	inputs, err := json.Marshal(f.r.scenario.inputs)
	if err != nil {
		t.Fatal("cold backup original signed inputs did not encode")
	}
	root, err := os.Lstat(f.Directory())
	if err != nil || !root.IsDir() || root.Mode()&os.ModeSymlink != 0 {
		t.Fatal("cold backup source root changed")
	}
	backup := &FullClientColdBackup{source: f.Directory(), directory: t.TempDir(), sourceRoot: root, inputs: sha256.Sum256(inputs)}
	backup.backupRoot, err = os.Lstat(backup.directory)
	if err != nil {
		t.Fatal("cold backup destination identity is unavailable")
	}
	backup.sourceEngine = f.engineDirectory
	names, err := fullClientColdNames(backup.source)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		entry, err := fullClientColdCopy(filepath.Join(backup.source, filepath.FromSlash(name)), filepath.Join(backup.directory, filepath.FromSlash(name)), fullClientColdByteLimit-backup.bytes)
		if err != nil {
			t.Fatal(err)
		}
		entry.Name = name
		backup.files = append(backup.files, entry)
		backup.bytes += entry.Bytes
		if strings.HasPrefix(name, "full-client/") {
			backup.engineFiles++
		} else if strings.HasPrefix(name, "node.db") {
			backup.sqlFiles++
		}
	}
	backup.seal = fullClientColdSeal(backup.files, backup.inputs)
	if backup.engineFiles == 0 || backup.sqlFiles < 1 || backup.sqlFiles > 4 || backup.verify(backup.directory) != nil || backup.verify(backup.source) != nil {
		t.Fatal("cold backup did not retain its complete bounded engine/SQLite pair")
	}
	t.Logf("full_client_paired_cold_backup captured=true files=%d engine_files=%d sqlite_files=%d config_files=4 logical_bytes=%d byte_limit=%d manifest_sha256=%x signed_inputs_sha256=%x source_readers_closed=true no_open_parent_engine=true", len(backup.files), backup.engineFiles, backup.sqlFiles, backup.bytes, fullClientColdByteLimit, backup.seal, backup.inputs)
	return backup
}

func (backup *FullClientColdBackup) restore(target *FullClientSignedRecoveryFixture) error {
	if err := backup.verify(backup.directory); err != nil {
		return err
	}
	entries, err := os.ReadDir(target.Directory())
	if err != nil || len(entries) != len(fullClientColdConfigs) || target.client != nil || target.admitted {
		return errors.New("cold restore refuses a nonempty persistence target")
	}
	allowed := map[string]bool{}
	for _, name := range fullClientColdConfigs {
		allowed[name] = true
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] || !entry.Type().IsRegular() {
			return errors.New("cold restore refuses existing or linked persistence")
		}
	}
	inputs, err := json.Marshal(target.r.scenario.inputs)
	if err != nil || sha256.Sum256(inputs) != backup.inputs {
		return errors.New("cold restore fixture signed inputs differ")
	}
	info, err := os.Lstat(target.Directory())
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || os.SameFile(info, backup.sourceRoot) || os.SameFile(info, backup.backupRoot) {
		return errors.New("cold restore target aliases or replaces its source")
	}
	for _, expected := range backup.files {
		if allowed[expected.Name] {
			digest, info, err := fullClientColdRead(filepath.Join(target.Directory(), expected.Name), io.Discard, 1<<20)
			if err != nil || digest != expected.SHA256 || info.Size() != expected.Bytes {
				return errors.New("cold restore configuration differs from its snapshot")
			}
		}
	}
	if err := target.read.Close(); err != nil {
		return errors.New("cold restore destination observer close failed")
	}
	target.read = nil
	for _, expected := range backup.files {
		if allowed[expected.Name] {
			continue
		}
		copied, err := fullClientColdCopy(filepath.Join(backup.directory, filepath.FromSlash(expected.Name)), filepath.Join(target.Directory(), filepath.FromSlash(expected.Name)), expected.Bytes)
		if err != nil || copied.Bytes != expected.Bytes || copied.SHA256 != expected.SHA256 {
			return errors.New("cold restore byte copy was incomplete or changed")
		}
	}
	if backup.verify(target.Directory()) != nil || backup.verify(backup.directory) != nil || backup.verify(backup.source) != nil {
		return errors.New("cold restore failed complete pair equality or changed retained inputs")
	}
	uri := url.URL{Scheme: "file", Path: filepath.Join(target.Directory(), "node.db"), RawQuery: "mode=ro"}
	target.read, err = sql.Open("sqlite", uri.String())
	if err != nil {
		return errors.New("cold restore read-only observer reopen failed")
	}
	target.read.SetMaxOpenConns(1)
	target.admitted = true // Existing signed rows came from the snapshot; Submit is not called.
	target.CheckExecutionClientDirectory()
	return nil
}

func RestoreFullClientSignedColdBackup(t *testing.T, backup *FullClientColdBackup) *FullClientSignedRecoveryFixture {
	t.Helper()
	// These isolated negative targets never open an engine or submit an order.
	badTarget := NewFullClientSignedProcessFixture(t)
	bad := *backup
	bad.files = append([]fullClientColdFile(nil), backup.files...)
	bad.files[0].SHA256[0] ^= 1
	if bad.restore(badTarget) == nil {
		t.Fatal("cold restore accepted a changed sealed manifest")
	}
	bad.seal = fullClientColdSeal(bad.files, bad.inputs)
	if bad.restore(badTarget) == nil {
		t.Fatal("cold restore trusted metadata without authenticating the retained bytes")
	}
	nonempty := NewFullClientSignedProcessFixture(t)
	sentinel := filepath.Join(nonempty.Directory(), "node.db")
	if err := os.WriteFile(sentinel, []byte("owned cold restore refusal sentinel"), 0600); err != nil {
		t.Fatal("cold restore refusal sentinel creation failed")
	}
	want := sha256.Sum256([]byte("owned cold restore refusal sentinel"))
	if backup.restore(nonempty) == nil {
		t.Fatal("cold restore overwrote a nonempty persistence target")
	}
	got, _, err := fullClientColdRead(sentinel, io.Discard, 1<<10)
	if err != nil || got != want {
		t.Fatal("cold restore refusal changed an existing target")
	}
	target := NewFullClientSignedProcessFixture(t)
	if err := backup.restore(target); err != nil {
		t.Fatal(err)
	}
	t.Logf("full_client_paired_cold_backup restored=true files=%d logical_bytes=%d manifest_sha256=%x destination_independent=true backup_unchanged=true source_unchanged=true configuration_matches=true signed_inputs_match=true manifest_refusal=true payload_refusal=true nonempty_target_refusal=true no_parent_repair=true", len(backup.files), backup.bytes, backup.seal)
	return target
}

func AssertFullClientColdBackupUnchanged(t *testing.T, backup *FullClientColdBackup) {
	t.Helper()
	if backup.verify(backup.directory) != nil || backup.verify(backup.source) != nil {
		t.Fatal("full-client recovery changed its retained cold backup or original source")
	}
	t.Logf("full_client_paired_cold_backup retained=true files=%d logical_bytes=%d manifest_sha256=%x after_recovery=true source_unchanged=true backup_unchanged=true", len(backup.files), backup.bytes, backup.seal)
}
