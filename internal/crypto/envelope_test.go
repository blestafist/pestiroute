package crypto

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestEnvelopeAuthenticationAndRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	secret := []byte("credential plaintext must stay protected")
	keyBytes := bytes.Repeat([]byte{0x5a}, KeySize)
	if err := os.WriteFile(path, keyBytes, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadMasterKey(path)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := Seal(loaded, 1, "key-v1", "credentials", "cred-7", "acct-2", secret)
	if err != nil {
		t.Fatal(err)
	}
	if len(envelope.Nonce) != NonceSize || envelope.FormatVersion != 1 || envelope.KeyVersion != "key-v1" {
		t.Fatalf("unexpected envelope metadata: %+v", envelope)
	}
	// A separate load models a process restart using the persisted external key.
	restarted, err := LoadMasterKey(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(restarted, envelope, "credentials", "cred-7", "acct-2")
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("Open() = %q, %v", got, err)
	}

	mutations := map[string]func(*Envelope){
		"ciphertext":     func(e *Envelope) { e.Ciphertext[0] ^= 1 },
		"tag":            func(e *Envelope) { e.Ciphertext[len(e.Ciphertext)-1] ^= 1 },
		"nonce":          func(e *Envelope) { e.Nonce[0] ^= 1 },
		"format version": func(e *Envelope) { e.FormatVersion++ },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := cloneEnvelope(envelope)
			mutate(&changed)
			if _, err := Open(restarted, changed, "credentials", "cred-7", "acct-2"); !errors.Is(err, ErrAuthentication) {
				t.Fatalf("Open() error = %v, want authentication error", err)
			}
		})
	}
	for _, tc := range []struct{ name, purpose, id, account string }{
		{"purpose", "auth_sessions", "cred-7", "acct-2"},
		{"record", "credentials", "cred-8", "acct-2"},
		{"account", "credentials", "cred-7", "acct-3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Open(restarted, envelope, tc.purpose, tc.id, tc.account); !errors.Is(err, ErrAuthentication) {
				t.Fatalf("Open() error = %v, want authentication error", err)
			}
		})
	}
	wrong := MasterKey{value: [KeySize]byte{1}}
	if _, err := Open(wrong, envelope, "credentials", "cred-7", "acct-2"); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong key error = %v, want authentication error", err)
	}
}

func TestEnvelopeNonceUniquenessAndRedaction(t *testing.T) {
	key := MasterKey{value: [KeySize]byte{1}}
	seen := map[string]bool{}
	for range 64 {
		envelope, err := Seal(key, 2, "v1", "credentials", "id", "account", []byte("private plaintext"))
		if err != nil {
			t.Fatal(err)
		}
		if seen[string(envelope.Nonce)] {
			t.Fatal("nonce repeated")
		}
		seen[string(envelope.Nonce)] = true
	}
	for _, formatted := range []string{fmt.Sprint(key), fmt.Sprintf("%#v", key), fmt.Sprintf("%+v", key)} {
		if bytes.Contains([]byte(formatted), key.value[:]) || formatted != "[REDACTED]" {
			t.Fatalf("key formatting is not redacted: %q", formatted)
		}
	}
	if _, err := Seal(key, 0, "v1", "credentials", "id", "account", []byte("private plaintext")); err == nil || bytes.Contains([]byte(err.Error()), []byte("private plaintext")) {
		t.Fatalf("invalid envelope error leaked data or succeeded: %v", err)
	}
}

func cloneEnvelope(e Envelope) Envelope {
	e.Nonce = append([]byte(nil), e.Nonce...)
	e.Ciphertext = append([]byte(nil), e.Ciphertext...)
	return e
}

func TestLoadMasterKeyRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid")
	if err := os.WriteFile(valid, bytes.Repeat([]byte{7}, KeySize), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(valid); err != nil {
		t.Fatalf("valid key: %v", err)
	}
	for _, tc := range []struct {
		name string
		mode os.FileMode
		data []byte
	}{
		{"short", 0600, bytes.Repeat([]byte{1}, KeySize-1)},
		{"long", 0600, bytes.Repeat([]byte{1}, KeySize+1)},
		{"0644", 0644, bytes.Repeat([]byte{1}, KeySize)},
		{"0660", 0660, bytes.Repeat([]byte{1}, KeySize)},
		{"0777", 0777, bytes.Repeat([]byte{1}, KeySize)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, tc.data, tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadMasterKey(path); !errors.Is(err, ErrKeyFile) {
				t.Fatalf("LoadMasterKey() error = %v, want ErrKeyFile", err)
			}
		})
	}
	if _, err := LoadMasterKey(filepath.Join(dir, "missing")); !errors.Is(err, ErrKeyFile) {
		t.Fatalf("missing-file error = %v, want ErrKeyFile", err)
	}
	directory := filepath.Join(dir, "directory")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(directory); !errors.Is(err, ErrKeyFile) {
		t.Fatalf("directory error = %v, want ErrKeyFile", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(link); !errors.Is(err, ErrKeyFile) {
		t.Fatalf("symlink error = %v, want ErrKeyFile", err)
	}
}

func TestLoadMasterKeyFIFORejectsWithoutBlocking(t *testing.T) {
	const helperEnv = "PESTIROUTE_TEST_FIFO_KEY"
	if path := os.Getenv(helperEnv); path != "" {
		if _, err := LoadMasterKey(path); !errors.Is(err, ErrKeyFile) {
			fmt.Fprintf(os.Stderr, "FIFO LoadMasterKey error = %v, want ErrKeyFile\n", err)
			os.Exit(1)
		}
		return
	}

	fifo := filepath.Join(t.TempDir(), "key.fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLoadMasterKeyFIFORejectsWithoutBlocking$")
	cmd.Env = append(os.Environ(), helperEnv+"="+fifo)
	if err := cmd.Run(); ctx.Err() != nil {
		t.Fatal("LoadMasterKey blocked opening a FIFO")
	} else if err != nil {
		t.Fatalf("FIFO subprocess failed: %v", err)
	}
}
