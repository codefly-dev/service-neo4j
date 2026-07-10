package main

// neo4jlog.go — structured rendering of the neo4j server log stream.
//
// Neo4j lines look like "2026-06-16 14:56:37.312+0000 INFO  Started." — a date,
// a timestamp with offset, a level word, then the message. Raw, every line
// lands at one undifferentiated level. A woollog.Writer parses each line with
// this declarative gortk.LogSpec and re-emits it through Wool at the mapped
// severity (the timestamp is dropped — Wool stamps its own).

import (
	"io"

	"github.com/codefly-dev/core/wool"
	"github.com/codefly-dev/core/woollog"
	"github.com/codefly-dev/gortk"
)

var neo4jLog = gortk.LogSpec{
	LineRegex: `^\d{4}-\d{2}-\d{2} \S+ (?P<level>INFO|WARN|WARNING|ERROR|DEBUG|TRACE)\s+(?P<msg>.*)$`,
	LevelMap: map[string]string{
		"INFO": "info", "WARN": "warn", "WARNING": "warn",
		"ERROR": "error", "DEBUG": "debug", "TRACE": "debug",
	},
	DefaultLevel: "info",
}

func newNeo4jLogWriter(w *wool.Wool) io.Writer {
	return woollog.MustNew(w, neo4jLog)
}
