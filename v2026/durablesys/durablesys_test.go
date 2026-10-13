//go:build linux || darwin

package durablesys

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const testAttribute = "user.urnetwork.durablesys-test"

func openTestDirectory(t *testing.T) (*os.File, string) {
	t.Helper()
	path := t.TempDir()
	directory, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { directory.Close() })
	return directory, path
}

func openTestFile(t *testing.T) *os.File {
	t.Helper()
	_, path := openTestDirectory(t)
	file, err := os.OpenFile(filepath.Join(path, "attributes"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file
}

func readTestAttribute(t *testing.T, file *os.File) ([]byte, error) {
	t.Helper()
	value := make([]byte, 64)
	n, err := GetAttribute(int(file.Fd()), testAttribute, value)
	if err != nil {
		return nil, err
	}
	return value[:n], nil
}

func TestRenameNoReplaceRefusesExistingTarget(t *testing.T) {
	directory, path := openTestDirectory(t)
	for name, contents := range map[string]string{"source": "source", "target": "target"} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	err := RenameNoReplace(int(directory.Fd()), "source", int(directory.Fd()), "target")
	if !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("no-replace rename onto an existing name returned %v, want EEXIST", err)
	}
	for name, contents := range map[string]string{"source": "source", "target": "target"} {
		raw, err := os.ReadFile(filepath.Join(path, name))
		if err != nil || string(raw) != contents {
			t.Fatalf("refused rename changed %s: %q %v", name, raw, err)
		}
	}
	if err := RenameNoReplace(int(directory.Fd()), "source", int(directory.Fd()), "fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(path, "source")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("renamed source still exists: %v", err)
	}
}

func TestRenameNoReplaceMovesDirectories(t *testing.T) {
	directory, path := openTestDirectory(t)
	if err := os.Mkdir(filepath.Join(path, "staged"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(path, "claimed"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := RenameNoReplace(int(directory.Fd()), "staged", int(directory.Fd()), "claimed"); !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("no-replace directory rename onto an existing directory returned %v, want EEXIST", err)
	}
	if err := RenameNoReplace(int(directory.Fd()), "staged", int(directory.Fd()), "published"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(path, "published")); err != nil || !info.IsDir() {
		t.Fatalf("published directory is absent: %v", err)
	}
}

func TestRenameExchangeSwapsNames(t *testing.T) {
	directory, path := openTestDirectory(t)
	for name, contents := range map[string]string{"left": "left", "right": "right"} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RenameExchange(int(directory.Fd()), "left", int(directory.Fd()), "right"); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{"left": "right", "right": "left"} {
		raw, err := os.ReadFile(filepath.Join(path, name))
		if err != nil || string(raw) != contents {
			t.Fatalf("exchange left %s as %q %v", name, raw, err)
		}
	}
	if err := RenameExchange(int(directory.Fd()), "left", int(directory.Fd()), "missing"); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("exchange with a missing name returned %v, want ENOENT", err)
	}
}

func TestSetAttributeCreateRefusesExistingAttribute(t *testing.T) {
	file := openTestFile(t)
	if err := SetAttribute(int(file.Fd()), testAttribute, []byte("original"), AttributeCreate); err != nil {
		t.Fatal(err)
	}
	err := SetAttribute(int(file.Fd()), testAttribute, []byte("replacement"), AttributeCreate)
	if !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("create over an existing attribute returned %v, want EEXIST", err)
	}
	value, err := readTestAttribute(t, file)
	if err != nil || !bytes.Equal(value, []byte("original")) {
		t.Fatalf("refused create changed the attribute to %q %v", value, err)
	}
}

func TestSetAttributeReplaceRefusesAbsentAttribute(t *testing.T) {
	file := openTestFile(t)
	err := SetAttribute(int(file.Fd()), testAttribute, []byte("enrolled"), AttributeReplace)
	if !errors.Is(err, ErrNoAttribute) {
		t.Fatalf("replace of an absent attribute returned %v, want ErrNoAttribute", err)
	}
	if _, err := readTestAttribute(t, file); !errors.Is(err, ErrNoAttribute) {
		t.Fatalf("refused replace enrolled an attribute: %v", err)
	}
	if err := SetAttribute(int(file.Fd()), testAttribute, []byte("first"), 0); err != nil {
		t.Fatal(err)
	}
	if err := SetAttribute(int(file.Fd()), testAttribute, []byte("second"), AttributeReplace); err != nil {
		t.Fatal(err)
	}
	if value, err := readTestAttribute(t, file); err != nil || string(value) != "second" {
		t.Fatalf("replace left %q %v", value, err)
	}
}

func TestSetAttributeRefusesCombinedConditions(t *testing.T) {
	file := openTestFile(t)
	if err := SetAttribute(int(file.Fd()), testAttribute, []byte("value"), AttributeCreate|AttributeReplace); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("combined create/replace returned %v, want EINVAL", err)
	}
	if _, err := readTestAttribute(t, file); !errors.Is(err, ErrNoAttribute) {
		t.Fatalf("refused write created an attribute: %v", err)
	}
}

func TestGetAttributeReportsAbsenceAndShortBuffer(t *testing.T) {
	file := openTestFile(t)
	if _, err := readTestAttribute(t, file); !errors.Is(err, ErrNoAttribute) {
		t.Fatalf("absent attribute returned %v, want ErrNoAttribute", err)
	}
	if err := SetAttribute(int(file.Fd()), testAttribute, []byte("exceeds"), AttributeCreate); err != nil {
		t.Fatal(err)
	}
	if _, err := GetAttribute(int(file.Fd()), testAttribute, make([]byte, 2)); !errors.Is(err, syscall.ERANGE) {
		t.Fatalf("short attribute buffer returned %v, want ERANGE", err)
	}
}

func TestListAttributesIncludesWrittenName(t *testing.T) {
	file := openTestFile(t)
	if err := SetAttribute(int(file.Fd()), testAttribute, []byte("listed"), AttributeCreate); err != nil {
		t.Fatal(err)
	}
	names := make([]byte, 64*1024)
	n, err := ListAttributes(int(file.Fd()), names)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range strings.Split(strings.TrimSuffix(string(names[:n]), "\x00"), "\x00") {
		found = found || name == testAttribute
	}
	if !found {
		t.Fatalf("attribute census %q lacks %q", names[:n], testAttribute)
	}
	if _, err := ListAttributes(int(file.Fd()), make([]byte, 1)); !errors.Is(err, syscall.ERANGE) {
		t.Fatalf("short name buffer returned %v, want ERANGE", err)
	}
}

// Descriptor and FileInfo observations agree and round-trip through the
// platform's own major/minor encoding without sign extension.
func TestStatDeviceMatchesFileInfoAndMkdev(t *testing.T) {
	file := openTestFile(t)
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	device := StatDevice(&stat)
	fromInfo, ok := FileInfoDevice(info)
	if !ok || fromInfo != device {
		t.Fatalf("file info device %#x %v, stat device %#x", fromInfo, ok, device)
	}
	if device == 0 || unix.Mkdev(unix.Major(device), unix.Minor(device)) != device {
		t.Fatalf("device %#x does not round-trip through Mkdev", device)
	}
}
