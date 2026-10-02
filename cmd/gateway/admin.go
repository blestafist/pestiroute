package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"syscall"

	secure "github.com/blestafist/pestiroute/internal/crypto"
	"github.com/blestafist/pestiroute/internal/storage/sqlite"
)

const adminUsage = "usage: gateway admin [--db PATH] [--master-key PATH] <migrate|status>\n"
const maxAdminSecret = 64 << 10

type adminEnvironment struct {
	ctx    context.Context
	args   []string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func runAdmin(env adminEnvironment) error {
	if env.ctx == nil {
		env.ctx = context.Background()
	}
	if env.stdout == nil {
		env.stdout = io.Discard
	}
	if env.stderr == nil {
		env.stderr = io.Discard
	}
	if len(env.args) == 0 || isHelp(env.args[0]) {
		_, _ = io.WriteString(env.stdout, adminUsage)
		return nil
	}
	if len(env.args) == 2 && isHelp(env.args[1]) && (env.args[0] == "migrate" || env.args[0] == "status") {
		_, _ = fmt.Fprintf(env.stdout, "%s  %s: no additional arguments\n", adminUsage, env.args[0])
		return nil
	}
	flags := flag.NewFlagSet("gateway admin", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dbPath := flags.String("db", "", "SQLite database path")
	keyPath := flags.String("master-key", "", "master-key file path")
	if err := flags.Parse(env.args); err != nil {
		_, _ = io.WriteString(env.stderr, adminUsage)
		return errors.New("admin: invalid arguments")
	}
	args := flags.Args()
	if len(args) == 2 && isHelp(args[1]) && (args[0] == "migrate" || args[0] == "status") {
		_, _ = fmt.Fprintf(env.stdout, "%s  %s: no additional arguments\n", adminUsage, args[0])
		return nil
	}
	if len(args) != 1 || isHelp(args[0]) {
		_, _ = io.WriteString(env.stdout, adminUsage)
		if len(args) == 1 {
			return nil
		}
		return errors.New("admin: expected one subcommand")
	}
	if args[0] != "migrate" && args[0] != "status" {
		_, _ = io.WriteString(env.stderr, adminUsage)
		return errors.New("admin: unknown subcommand")
	}
	if *dbPath == "" || *keyPath == "" {
		_, _ = io.WriteString(env.stderr, adminUsage)
		return errors.New("admin: --db and --master-key are required")
	}
	key, err := secure.LoadMasterKey(*keyPath)
	if err != nil {
		return errors.New("admin: invalid master key file")
	}
	_ = key // The boundary validates the external key; future secret-consuming commands own its use.
	db, err := openAdminDatabase(*dbPath, args[0] == "migrate")
	if err != nil {
		return errors.New("admin: cannot open database")
	}
	defer db.Close()
	if args[0] == "migrate" {
		if err := sqlite.Migrate(env.ctx, db); err != nil {
			return errors.New("admin: migration failed")
		}
		_, _ = fmt.Fprintln(env.stdout, "schema is current")
		return nil
	}
	version, err := sqlite.SchemaVersion(env.ctx, db)
	if err != nil {
		return errors.New("admin: unsupported or unreadable schema")
	}
	_, _ = fmt.Fprintf(env.stdout, "schema version %d\n", version)
	return nil
}

func isHelp(arg string) bool { return arg == "--help" || arg == "-h" || arg == "help" }

func openAdminDatabase(path string, create bool) (*sql.DB, error) {
	flags := syscall.O_RDWR | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	fd, err := syscall.Open(path, flags, 0)
	if errors.Is(err, syscall.ENOENT) && create {
		fd, err = syscall.Open(path, flags|syscall.O_CREAT|syscall.O_EXCL, 0600)
	}
	if err != nil {
		return nil, errors.New("database file unavailable")
	}
	defer syscall.Close(fd)
	var opened syscall.Stat_t
	if err := syscall.Fstat(fd, &opened); err != nil || opened.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return nil, errors.New("database must be a regular file")
	}
	db, err := sqlite.Open(path)
	if err != nil {
		return nil, err
	}
	var current syscall.Stat_t
	if err := syscall.Lstat(path, &current); err != nil || current.Mode&syscall.S_IFMT != syscall.S_IFREG || current.Dev != opened.Dev || current.Ino != opened.Ino {
		_ = db.Close()
		return nil, errors.New("database path changed while opening")
	}
	return db, nil
}

func readAdminSecret(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, errors.New("admin: secret input unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(r, maxAdminSecret+1))
	if err != nil {
		zeroAdminSecret(data)
		return nil, errors.New("admin: cannot read secret input")
	}
	if len(data) > maxAdminSecret {
		zeroAdminSecret(data)
		return nil, errors.New("admin: secret input exceeds 64 KiB")
	}
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	return data, nil
}

func zeroAdminSecret(secret []byte) {
	for i := range secret {
		secret[i] = 0
	}
}
