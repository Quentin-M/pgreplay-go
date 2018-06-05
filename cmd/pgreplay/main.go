package main

import (
	stdlog "log"
	"os"
	"time"

	"github.com/alecthomas/kingpin"
	kitlog "github.com/go-kit/kit/log"
	level "github.com/go-kit/kit/log/level"
	pgreplay "github.com/gocardless/pgreplay-go"

	"github.com/jackc/pgx"
)

var logger kitlog.Logger

var (
	host         = kingpin.Flag("host", "PostgreSQL database host").Required().String()
	port         = kingpin.Flag("port", "PostgreSQL database port").Default("5432").Uint16()
	datname      = kingpin.Flag("database", "PostgreSQL root database").Default("postgres").String()
	user         = kingpin.Flag("user", "PostgreSQL root user").Default("postgres").String()
	errlogFile   = kingpin.Flag("errlog-file", "Path to PostgreSQL errlog").Required().ExistingFile()
	debug        = kingpin.Flag("debug", "Enable debug logging").Default("false").Bool()
	pollInterval = kingpin.Flag("poll-interval", "Interval between polling for finish").Default("5s").Duration()
)

func main() {
	kingpin.Parse()

	logger = kitlog.NewLogfmtLogger(kitlog.NewSyncWriter(os.Stderr))
	logger = level.NewFilter(logger, level.AllowInfo())

	if *debug {
		logger = level.NewFilter(logger, level.AllowDebug())
	}

	logger = kitlog.With(logger, "ts", kitlog.DefaultTimestampUTC, "caller", kitlog.DefaultCaller)
	stdlog.SetOutput(kitlog.NewStdlibAdapter(logger))

	errlog, err := os.Open(*errlogFile)
	if err != nil {
		logger.Log("event", "logfile.error", "error", err)
		os.Exit(255)
	}

	database, err := pgreplay.NewDatabase(pgx.ConnConfig{
		Host:     *host,
		Port:     *port,
		Database: *datname,
		User:     *user,
	})

	if err != nil {
		logger.Log("event", "postgres.error", "error", err)
		os.Exit(255)
	}

	items, logerrs, done := pgreplay.Parse(errlog)

	go func() {
		logger.Log("event", "parse.finished", "error", <-done)
	}()

	go func() {
		for err := range logerrs {
			level.Debug(logger).Log("event", "parse.error", "error", err)
		}
	}()

	errs, consumeDone := database.Consume(items)
	poller := time.NewTicker(*pollInterval)

	progress := func(logger kitlog.Logger) kitlog.Logger {
		return kitlog.With(logger, "consumed", database.Consumed(), "latest", database.Latest())
	}

	var status int

	for {
		select {
		case err := <-errs:
			if err != nil {
				logger.Log("event", "consume.error", "error", err)
			}
		case err := <-consumeDone:
			if err != nil {
				status = 255
			}

			progress(logger).Log("event", "consume.finished", "error", err, "status", status)
			os.Exit(status)

		// Poll our consumer to determine how much work remains
		case <-poller.C:
			if conns, pending := database.Pending(); pending > 0 {
				progress(logger).Log("event", "consume.pending", "connections", len(conns), "items", pending)
			}
		}
	}
}