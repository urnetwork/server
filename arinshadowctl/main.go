// arinshadowctl consumes an explicitly attested census and ephemeral lookup
// input. It never contacts Main, publishes resources, or activates the guard.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	server "github.com/urnetwork/server"
	"io"
	"os"
	"time"
)

const limit = 32 << 20

var invalid = errors.New("shadow input unavailable or inconsistent")

type census struct {
	Complete        bool             `json:"complete"`
	RequiredBuckets []string         `json:"required_buckets"`
	Providers       []censusProvider `json:"providers"`
}
type censusProvider struct {
	Token         string                                  `json:"token"`
	Connections   []string                                `json:"connections"`
	Lookups       map[string]server.ArinShadowActiveFacts `json:"lookups"`
	Buckets       []string                                `json:"buckets"`
	BaseQuality   *bool                                   `json:"base_quality"`
	BaseSpeed     *bool                                   `json:"base_speed"`
	ActiveQuality *bool                                   `json:"active_quality"`
	ActiveSpeed   *bool                                   `json:"active_speed"`
}

func (c census) providers() ([]server.ArinShadowProvider, error) {
	result := make([]server.ArinShadowProvider, 0, len(c.Providers))
	for _, p := range c.Providers {
		if p.BaseQuality == nil || p.BaseSpeed == nil || p.ActiveQuality == nil || p.ActiveSpeed == nil {
			return nil, invalid
		}
		result = append(result, server.ArinShadowProvider{Token: p.Token, Connections: p.Connections, Lookups: p.Lookups, Buckets: p.Buckets, BaseQuality: *p.BaseQuality, BaseSpeed: *p.BaseSpeed, ActiveQuality: *p.ActiveQuality, ActiveSpeed: *p.ActiveSpeed})
	}
	return result, nil
}

type observation struct {
	Connection string                        `json:"connection"`
	Address    string                        `json:"address"`
	Active     *server.ArinShadowActiveFacts `json:"active"`
}

func decode(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return invalid
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return invalid
	}
	return nil
}
func run(input io.Reader, output io.Writer, args []string) error {
	flags := flag.NewFlagSet("arinshadowctl", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	active := flags.String("active-mmdb", "", "")
	activePin := flags.String("active-sha256", "", "")
	candidate := flags.String("candidate-mmdb", "", "")
	candidatePin := flags.String("candidate-sha256", "", "")
	censusPath := flags.String("census", "", "")
	censusPin := flags.String("census-sha256", "", "")
	startText := flags.String("cutover", "", "")
	capacity := flags.Int("capacity", 100000, "")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return invalid
	}
	start, err := time.Parse(time.RFC3339Nano, *startText)
	if err != nil || start.After(time.Now()) || time.Since(start) > 90*time.Second {
		return invalid
	}
	file, err := os.Open(*censusPath)
	if err != nil {
		return invalid
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(data) > limit {
		return invalid
	}
	pin, err := hex.DecodeString(*censusPin)
	if err != nil || len(pin) != 32 {
		return invalid
	}
	sum := sha256.Sum256(data)
	if !bytes.Equal(sum[:], pin) {
		return invalid
	}
	var providers census
	if decode(data, &providers) != nil {
		return invalid
	}
	rows, err := providers.providers()
	if err != nil {
		return invalid
	}
	recorder, err := server.OpenArinShadowRecorder(*active, *activePin, *candidate, *candidatePin, start, *capacity)
	if err != nil {
		return invalid
	}
	defer recorder.Close()
	scanner := bufio.NewScanner(io.LimitReader(input, limit+1))
	scanner.Buffer(make([]byte, 4096), 4096)
	total, count := 0, 0
	for scanner.Scan() {
		total += len(scanner.Bytes()) + 1
		count++
		if total > limit || count > *capacity || time.Since(start) > 90*time.Second {
			return invalid
		}
		var row observation
		if decode(scanner.Bytes(), &row) != nil || row.Active == nil {
			return invalid
		}
		recorder.Observe(row.Connection, row.Address, *row.Active)
	}
	if scanner.Err() != nil || time.Since(start) > 90*time.Second {
		return invalid
	}
	report, err := recorder.Snapshot(rows, providers.RequiredBuckets, providers.Complete, 90*time.Second)
	if err != nil {
		return invalid
	}
	return json.NewEncoder(output).Encode(report)
}
func main() {
	var err error
	if len(os.Args) > 1 && os.Args[1] == "capture-assemble" {
		err = runCaptureAssemble(os.Stdout, os.Args[2:])
	} else if len(os.Args) > 1 && os.Args[1] == "capture-host-inventory" {
		err = runCaptureHostInventory(os.Stdout, os.Args[2:])
	} else if len(os.Args) > 1 && os.Args[1] == "capture-prepare" {
		err = runCapturePrepare(os.Stdout, os.Args[2:])
	} else if len(os.Args) > 1 && os.Args[1] == "capture-current" {
		err = runCaptureCurrent(os.Stdout, os.Args[2:])
	} else if len(os.Args) > 1 && os.Args[1] == "current-cause" {
		err = runCurrentCause(os.Stdout, os.Args[2:])
	} else if len(os.Args) > 1 && os.Args[1] == "capture-bridge" {
		err = runCaptureBridge(os.Stdin, os.Stdout, os.Args[2:])
	} else {
		err = run(os.Stdin, os.Stdout, os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ARIN shadow input unavailable or inconsistent")
		os.Exit(1)
	}
}
