package rwfs

import (
	"testing"
)

func TestOS(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	t.Logf("tempdir: %s", tempDir)
	fs := OSNew(tempDir)
	if fs == nil {
		t.Errorf("OSNew returned nil")
		return
	}
	testRWFS(t, fs)

	fsOS := OSNew("")
	f, err := fsOS.Open("..")
	if err != nil {
		t.Errorf("failed opening relative dir: %v", err)
	} else {
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			t.Errorf("failed stat on relative dir: %v", err)
		}
		if !fi.IsDir() {
			t.Errorf("relative dir is not a directory")
		}
	}
	// attempt to escape tempdir
	fsCur := OSNew(".")
	f, err = fsCur.Open("..")
	if err == nil {
		t.Errorf("opened relative dir")
		defer f.Close()
	}
}
