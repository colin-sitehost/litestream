package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"

	"github.com/benbjohnson/litestream"
	"github.com/benbjohnson/litestream/abs"
	"github.com/benbjohnson/litestream/file"
	"github.com/benbjohnson/litestream/gcs"
	"github.com/benbjohnson/litestream/http"
	"github.com/benbjohnson/litestream/s3"
	"github.com/benbjohnson/litestream/sftp"
	"github.com/mattn/go-shellwords"
)

// ReplicateCommand represents a command that continuously replicates SQLite databases.
type ReplicateCommand struct {
	cmd        *exec.Cmd  // subcommand
	execCh     chan error // subcommand error channel
	httpServer *http.Server

	Config Config

	// List of managed databases specified in the config.
	DBs []*litestream.DB
}

func NewReplicateCommand() *ReplicateCommand {
	return &ReplicateCommand{
		execCh: make(chan error),
	}
}

// ParseFlags parses the CLI flags and loads the configuration file.
func (c *ReplicateCommand) ParseFlags(ctx context.Context, args []string) (err error) {
	fs := flag.NewFlagSet("litestream-replicate", flag.ContinueOnError)
	execFlag := fs.String("exec", "", "execute subcommand")
	configPath, noExpandEnv := registerConfigFlag(fs)
	fs.Usage = c.Usage
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Load configuration or use CLI args to build db/replica.
	if fs.NArg() == 1 {
		return fmt.Errorf("must specify at least one replica URL for %s", fs.Arg(0))
	} else if fs.NArg() > 1 {
		if *configPath != "" {
			return fmt.Errorf("cannot specify a replica URL and the -config flag")
		}

		dbConfig := &DBConfig{Path: fs.Arg(0)}
		for _, u := range fs.Args()[1:] {
			syncInterval := litestream.DefaultSyncInterval
			dbConfig.Replicas = append(dbConfig.Replicas, &ReplicaConfig{
				URL:          u,
				SyncInterval: &syncInterval,
			})
		}
		c.Config.DBs = []*DBConfig{dbConfig}
	} else {
		if *configPath == "" {
			*configPath = DefaultConfigPath()
		}
		if c.Config, err = ReadConfigFile(*configPath, !*noExpandEnv); err != nil {
			return err
		}
	}

	// Override config exec command, if specified.
	if *execFlag != "" {
		c.Config.Exec = *execFlag
	}

	return nil
}

// Run loads all databases specified in the configuration.
func (c *ReplicateCommand) Run(ctx context.Context) (err error) {
	// Display version information.
	log.Printf("litestream %s", Version)

	// Setup databases.
	if len(c.Config.DBs) == 0 {
		log.Println("no databases specified in configuration")
	}

	for _, dbConfig := range c.Config.DBs {
		db, err := NewDBFromConfig(dbConfig)
		if err != nil {
			return err
		}

		// Open database & attach to program.
		if err := db.Open(); err != nil {
			return err
		}
		c.DBs = append(c.DBs, db)
	}

	// Notify user that initialization is done.
	for _, db := range c.DBs {
		log.Printf("initialized db: %s", db.Path())
		for _, r := range db.Replicas {
			switch client := r.Client.(type) {
			case *file.ReplicaClient:
				log.Printf("replicating to: name=%q type=%q path=%q", r.Name(), client.Type(), client.Path())
			case *s3.ReplicaClient:
				log.Printf("replicating to: name=%q type=%q bucket=%q path=%q region=%q endpoint=%q sync-interval=%s", r.Name(), client.Type(), client.Bucket, client.Path, client.Region, client.Endpoint, r.SyncInterval)
			case *gcs.ReplicaClient:
				log.Printf("replicating to: name=%q type=%q bucket=%q path=%q sync-interval=%s", r.Name(), client.Type(), client.Bucket, client.Path, r.SyncInterval)
			case *abs.ReplicaClient:
				log.Printf("replicating to: name=%q type=%q bucket=%q path=%q endpoint=%q sync-interval=%s", r.Name(), client.Type(), client.Bucket, client.Path, client.Endpoint, r.SyncInterval)
			case *sftp.ReplicaClient:
				log.Printf("replicating to: name=%q type=%q host=%q user=%q path=%q sync-interval=%s", r.Name(), client.Type(), client.Host, client.User, client.Path, r.SyncInterval)
			default:
				log.Printf("replicating to: name=%q type=%q", r.Name(), client.Type())
			}
		}
	}

	// Run HTTP server, if enabled.
	if c.Config.Addr != "" {
		c.httpServer = http.NewServer(c.Config.Addr)
		c.httpServer.DB = c.DBs[0] // TEMP: Refactor to use any database
		if err := c.httpServer.Open(); err != nil {
			return fmt.Errorf("cannot start http server: %w", err)
		}

		log.Printf("serving http requests on %s", c.httpServer.URL())
	}

	// Parse exec commands args & start subprocess.
	if c.Config.Exec != "" {
		execArgs, err := shellwords.Parse(c.Config.Exec)
		if err != nil {
			return fmt.Errorf("cannot parse exec command: %w", err)
		}

		c.cmd = exec.CommandContext(ctx, execArgs[0], execArgs[1:]...)
		c.cmd.Env = os.Environ()
		c.cmd.Stdout = os.Stdout
		c.cmd.Stderr = os.Stderr
		if err := c.cmd.Start(); err != nil {
			return fmt.Errorf("cannot start exec command: %w", err)
		}
		go func() { c.execCh <- c.cmd.Wait() }()
	}

	return nil
}

// Close closes all open databases.
func (c *ReplicateCommand) Close() (err error) {
	if c.httpServer != nil {
		if e := c.httpServer.Close(); e != nil && err == nil {
			err = e
		}
	}

	for _, db := range c.DBs {
		if e := db.SoftClose(); e != nil {
			log.Printf("error closing db: path=%s err=%s", db.Path(), e)
			if err == nil {
				err = e
			}
		}
	}
	return err
}

// Usage prints the help screen to STDOUT.
func (c *ReplicateCommand) Usage() {
	fmt.Printf(`
The replicate command starts a server to monitor & replicate databases. 
You can specify your database & replicas in a configuration file or you can
replicate a single database file by specifying its path and its replicas in the
command line arguments.

Usage:

	litestream replicate [arguments]

	litestream replicate [arguments] DB_PATH REPLICA_URL [REPLICA_URL...]

Arguments:

	-config PATH
	    Specifies the configuration file.
	    Defaults to %s

	-exec CMD
	    Executes a subcommand. Litestream will exit when the child
	    process exits. Useful for simple process management.

	-no-expand-env
	    Disables environment variable expansion in configuration file.

`[1:], DefaultConfigPath())
}
