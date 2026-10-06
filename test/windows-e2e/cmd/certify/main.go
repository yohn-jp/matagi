package main

import (
	"flag"
	"fmt"
	"github.com/yohn-jp/matagi/test/windows-e2e/identity"
	"os"
	"path/filepath"
)

func main() {
	dir := flag.String("candidate", "candidate", "")
	evidence := flag.String("evidence", "evidence", "")
	source := flag.String("source", "", "")
	sum := flag.String("sha256", "", "")
	shard := flag.String("shard", "", "")
	jobs := flag.Bool("jobs-ok", false, "")
	out := flag.String("out", "certification.json", "")
	flag.Parse()
	var err error
	switch flag.Arg(0) {
	case "record":
		var c identity.Candidate
		c, err = identity.Record(*dir, *source)
		if err == nil {
			fmt.Printf("sha256=%s\nsource=%s\n", c.SHA256, c.Source)
		}
	case "verify":
		_, err = identity.Verify(*dir, *source, *sum)
	case "aggregate":
		var c identity.Certification
		c, err = identity.Aggregate(*dir, *evidence, *source, *sum, *jobs)
		if err == nil {
			err = identity.SaveCertification(*out, c)
		}
	case "release-check":
		err = identity.CheckCertification(*out, *dir, *source, *sum)
	case "evidence":
		_, err = identity.Verify(*dir, *source, *sum)
		if err == nil {
			if *shard == "" {
				err = fmt.Errorf("missing shard")
			} else {
				err = identity.RecordEvidence(filepath.Join(*evidence, *shard, "result.json"), identity.Evidence{Shard: *shard, Source: *source, SHA256: *sum, Passed: true})
			}
		}
	default:
		err = fmt.Errorf("unknown command")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
