//go:build linux

// A synthetic signed agent and an independently controlled pipe-holding child.
// It never loads installed configuration, contacts a host or starts evaluation.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/urnetwork/server/competitionagent"
)

// The test alone owns the generated keys, abstract socket and fixed behavior.
type fixtureConfig struct {
	Socket                  string    `json:"socket"`
	Mode                    string    `json:"mode"`
	Hold                    string    `json:"hold"`
	PrivateKeyBase64        string    `json:"private_key_base64"`
	CoordinatorPublicBase64 string    `json:"coordinator_public_base64"`
	Artifact                []byte    `json:"artifact"`
	DeadlineAt              time.Time `json:"deadline_at"`
}

// This separate control connection never carries evaluator protocol bytes.
type controlEvent struct {
	Event string `json:"event"`
	Pid   int    `json:"pid,omitempty"`
}

// Commands are test barriers, not timing assumptions about process scheduling.
type controlCommand struct {
	Command string `json:"command"`
}

// Only synthetic command shapes can enter either side of the fixture.
func main() {
	var err error
	if len(os.Args) == 5 && os.Args[1] == "--hold-streams" {
		err = holdStreams(os.Args[2], os.Args[3], os.Args[4])
	} else if len(os.Args) == 4 && os.Args[1] == "--config" && os.Args[3] == "--serve-managed" {
		err = serve(os.Args[2])
	} else {
		err = errors.New("unexpected fixture command")
	}
	if err != nil {
		// Deliberately do not echo configuration, request or private keys.
		os.Exit(2)
	}
}

// The holder remains alive until its test owner explicitly joins its exit.
func holdStreams(socket, streams, deadlineText string) error {
	deadline, err := time.Parse(time.RFC3339Nano, deadlineText)
	if err != nil || !time.Now().Before(deadline) {
		return errors.New("fixture holder has no live control deadline")
	}
	connection, err := net.Dial("unix", socket)
	if err != nil {
		return err
	}
	defer connection.Close()
	if err := connection.SetDeadline(deadline); err != nil {
		return err
	}
	encoder, decoder := json.NewEncoder(connection), json.NewDecoder(connection)
	if err := encoder.Encode(controlEvent{Event: "holder-ready", Pid: os.Getpid()}); err != nil {
		return err
	}
	for {
		command := controlCommand{}
		if err := decoder.Decode(&command); err != nil {
			return err
		}
		switch command.Command {
		case "ping":
			if err := encoder.Encode(controlEvent{Event: "holder-alive"}); err != nil {
				return err
			}
		case "release":
			return nil
		default:
			return errors.New("unexpected holder command")
		}
	}
}

// All held descriptors are real inherited standard streams. The direct child
// exits without waiting, reproducing an inherited pipe after exact-child exit.
func startHolder(config fixtureConfig) error {
	if config.Hold == "" {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	process := exec.Command(executable, "--hold-streams", config.Socket, config.Hold, config.DeadlineAt.Format(time.RFC3339Nano))
	if strings.Contains(config.Hold, "stdin") {
		process.Stdin = os.Stdin
	}
	if strings.Contains(config.Hold, "stdout") {
		process.Stdout = os.Stdout
	}
	if strings.Contains(config.Hold, "stderr") {
		process.Stderr = os.Stderr
	}
	if err := process.Start(); err != nil {
		return err
	}
	return process.Process.Release()
}

// Uses production document framing and signatures. Artifact bodies are raw
// production-format concatenation, deliberately truncated for the fault modes.
func serve(configPath string) error {
	content, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	config := fixtureConfig{}
	if err := json.Unmarshal(content, &config); err != nil {
		return err
	}
	connection, err := net.Dial("unix", config.Socket)
	if err != nil {
		return err
	}
	defer connection.Close()
	if !time.Now().Before(config.DeadlineAt) {
		return errors.New("fixture has no live control deadline")
	}
	if err := connection.SetDeadline(config.DeadlineAt); err != nil {
		return err
	}
	encoder, decoder := json.NewEncoder(connection), json.NewDecoder(connection)
	if err := encoder.Encode(controlEvent{Event: "agent-ready", Pid: os.Getpid()}); err != nil {
		return err
	}
	command := controlCommand{}
	if err := decoder.Decode(&command); err != nil || command.Command != "proceed" {
		return errors.New("fixture was not released")
	}
	request := competitionagent.Request{}
	limits := competitionagent.DefaultLimits()
	if err := competitionagent.ReadDocument(os.Stdin, &request, limits.DocumentBytes); err != nil {
		return err
	}
	coordinatorKey, err := base64.StdEncoding.DecodeString(config.CoordinatorPublicBase64)
	if err != nil || competitionagent.VerifyRequest(request, ed25519.PublicKey(coordinatorKey)) != nil {
		return errors.New("synthetic request did not authenticate")
	}
	if err := startHolder(config); err != nil {
		return err
	}
	response := competitionagent.Response{Schema: competitionagent.Schema, HostId: request.HostId, RunId: request.RunId, FencingToken: request.FencingToken, State: "succeeded"}
	if config.Mode == "wrong-fence" {
		response.FencingToken++
	}
	if request.Operation == "artifacts" {
		digest := sha256.Sum256(config.Artifact)
		response.Files = []competitionagent.File{{Path: "synthetic-artifact.bin", Bytes: int64(len(config.Artifact)), Sha256: hex.EncodeToString(digest[:])}}
	}
	privateKey, err := base64.StdEncoding.DecodeString(config.PrivateKeyBase64)
	if err != nil || competitionagent.SignResponse(&response, ed25519.PrivateKey(privateKey)) != nil {
		return errors.New("synthetic response did not sign")
	}
	if config.Mode == "bad-signature" {
		response.Signature = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	}
	var wire bytes.Buffer
	if err := competitionagent.WriteDocument(&wire, response, limits.DocumentBytes); err != nil {
		return err
	}
	switch config.Mode {
	case "before-response":
	case "partial-document":
		_, err = os.Stdout.Write(wire.Bytes()[:4])
	case "partial-document-body":
		_, err = os.Stdout.Write(wire.Bytes()[:8+(wire.Len()-8)/2])
	default:
		_, err = io.Copy(os.Stdout, &wire)
	}
	if err != nil {
		return err
	}
	if request.Operation == "artifacts" && config.Mode != "before-response" && config.Mode != "partial-document" && config.Mode != "partial-document-body" {
		artifact := config.Artifact
		if config.Mode == "partial-file" {
			artifact = artifact[:len(artifact)/2]
		} else if config.Mode == "corrupt-file" {
			artifact = bytes.Repeat([]byte("!"), len(artifact))
		}
		if _, err := os.Stdout.Write(artifact); err != nil {
			return err
		}
	}
	if config.Mode == "trailing-byte" {
		if _, err := os.Stdout.Write([]byte{1}); err != nil {
			return err
		}
	}
	if config.Mode == "diagnostic-overflow" {
		if _, err := os.Stderr.Write(bytes.Repeat([]byte("private synthetic diagnostic"), 4096)); err != nil {
			return err
		}
	}
	if err := encoder.Encode(controlEvent{Event: "response-emitted"}); err != nil {
		return err
	}
	command = controlCommand{}
	if err := decoder.Decode(&command); err != nil || command.Command != "exit" {
		return errors.New("fixture exit was not released")
	}
	if config.Mode == "exit-failure" {
		os.Exit(7)
	}
	return nil
}
