package session

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/klauspost/compress/zstd"

	"github.com/bradfitz/tsterm/pkg/vt"
)

// A session with a state directory saves its screen there every
// screenSaveInterval while it changes, and when it ends, so that what was
// on it survives a crash of tstermd, the shim, or the whole system. The
// lines that scrolled off it are in its history.

// screenSaveInterval is how often a session saves its screen, if it has
// changed. Tests lower it.
var screenSaveInterval = 5 * time.Second

// screenFile is the name of the saved screen in a session's directory: the
// zstd-compressed CBOR of a SavedScreen.
const screenFile = "screen"

// SavedScreen is a session's screen as last saved.
type SavedScreen struct {
	Saved  time.Time `cbor:"t"`
	Screen vt.Screen `cbor:"s"` // its History is the history's length then
}

var (
	screenEnc = sync.OnceValue(func() *zstd.Encoder {
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			panic(err)
		}
		return enc
	})
	screenDec = sync.OnceValue(func() *zstd.Decoder {
		dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(64<<20))
		if err != nil {
			panic(err)
		}
		return dec
	})
)

// LoadScreen returns the screen last saved in the session directory dir.
func LoadScreen(dir string) (SavedScreen, error) {
	var ss SavedScreen
	b, err := os.ReadFile(filepath.Join(dir, screenFile))
	if err != nil {
		return ss, err
	}
	dec, err := screenDec().DecodeAll(b, nil)
	if err != nil {
		return ss, err
	}
	err = cbor.Unmarshal(dec, &ss)
	return ss, err
}

// saveScreenLoop saves the screen every screenSaveInterval if it has
// changed, until stopSaving.
func (s *Session) saveScreenLoop() {
	defer close(s.saverDone)
	t := time.NewTicker(screenSaveInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stopSaver:
			return
		case <-t.C:
			if s.screenDirty.Swap(false) {
				s.saveScreen()
			}
		}
	}
}

// stopSaving stops saveScreenLoop and saves the screen a last time if it
// has changed.
func (s *Session) stopSaving() {
	if s.dir == "" {
		return
	}
	s.stopSaverOnce.Do(func() { close(s.stopSaver) })
	<-s.saverDone
	if s.screenDirty.Swap(false) {
		s.saveScreen()
	}
}

// saveScreen saves the screen, after syncing the history up to it, so that
// after a crash of the system the two agree.
func (s *Session) saveScreen() {
	scr := s.Snapshot()
	if s.hist != nil {
		if err := s.hist.Sync(); err != nil {
			s.logf("session %s: syncing history: %v", s.id, err)
		}
	}
	b, err := cbor.Marshal(SavedScreen{Saved: time.Now(), Screen: scr})
	if err != nil {
		s.logf("session %s: saving screen: %v", s.id, err)
		return
	}
	if err := writeFileSync(filepath.Join(s.dir, screenFile), screenEnc().EncodeAll(b, nil)); err != nil {
		s.logf("session %s: saving screen: %v", s.id, err)
	}
	if s.Active() {
		s.saveInfo(time.Time{}, "") // for its activity times
	}
}

// writeFileSync replaces the file at path with data, atomically and
// durably: it writes a temporary file, syncs it, renames it over path, and
// syncs the directory.
func writeFileSync(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
