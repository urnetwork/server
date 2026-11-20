// `sidecarctl internal bearer-keygen --class=<class> --out=<file> --verify=<file>` draws one caller class's bearer for
// api's internal gate (QD-1102): the random bits a service sends api's internal routes, in the scheme of QD-027 and
// QD-055 that server/internal_gate.go implements. It serves QD-812's edge class first, and any class that file knows,
// the platform listener's mxedge-platform among them (QD-1121); the class list is internal_gate.go's own. The owner
// runs it on a machine of their own, on files alone: no env, no vault and no network. It writes exactly what
// internal_gate.go reads:
//
//   - --out names the class's credential file, which vault holds as internal-gate-<class>.yml for the services that
//     mount it: `current: <bearer>`, and `previous: <bearer>` during a rotation, each bearer BearerByteCount bytes
//     from crypto/rand in unpadded base64url. When a file stands at --out the run is a rotation: the new bearer
//     becomes current, the old current previous, and the old previous is dropped. When none does, the file is the
//     class's first, holding a current alone.
//   - --verify names api's verify file, which vault holds as internal-gate-verify.yml: a map of caller names, each
//     holding its class and the SHA-256, in hex, of each bearer it may send. The class's entry is set to the new
//     bearer's digest and, in a rotation, the old current's, so api takes both while the services move. Every other
//     entry is kept as it was, with its comments. When no file stands at --verify one is made holding the class's
//     entry alone.
//
// The two files must be a pair before the run, as one environment's vault holds them. A credential file at --out
// needs the verify file's entry of its class to hold its current bearer's digest, and no other class's entry to; a
// verify file holding an entry of the class needs the class's credential file at --out. Anything else is refused,
// since a rotation from files that are no pair would give api the digest of a bearer no service sends, or drop one a
// service does. So is a file either of internal_gate.go's parsers refuses, a symlink or anything else that is not a
// regular file, a verify file holding two entries of the class or written with YAML anchors or more than one
// document, and a command line naming one file twice or a file by another file's vault name.
//
// Before anything is written, both new files are read back through internal_gate.go's own parser and verifier: the
// credential file must send the new bearer and keep the old current as previous, api's verifier must take the new
// bearer, and the old current in a rotation, as the class's, and every other entry must read as it did. Each file is
// written beside its path, 0600 and synced, and then put in place, the verify file first. A run that cannot put the
// credential file in place puts the verify file back, so the paths hold both new files or both old ones. A run killed
// between the two leaves the new verify file beside the old credential file: in a rotation they still pair, and api
// takes the bearer the services send, so the next run goes on from them; after a first draw the class's entry is
// removed by hand before the next. Neither bearer, nor any digest, reaches stdout, stderr or a log, and no message
// quotes the YAML parser, whose words can quote a file: the output names the files, classes and entries alone.
//
// To deploy, the owner places the verify file in vault and deploys api first, which reads it once at start and then
// takes both bearers, and then places the credential file and deploys the services that mount it, which read it once
// at start. Two runs drop a bearer from both files, which is how one thought stolen leaves them.

package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
)

// The command's registration. It is the only change to the binary this command needs.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words: "internal bearer-keygen",
		usage: "--class=<" + internalBearerKeygenClassText("|") + "> --out=<file> --verify=<file>",
		summary: "Draw one caller class's bearer for api's internal gate (QD-1102): --out gets the class's credential file, " +
			"internal-gate-<class>.yml for vault, the new bearer current and, when a file stands there already, its current " +
			"previous; --verify gets api's " + server.InternalGateVerifyFileName + ", the class's entry holding both bearers' " +
			"SHA-256 and every other entry kept. Both are written 0600 and no bearer is printed. A rotation is the same command " +
			"on the files vault holds: place the verify file and deploy api first, then the credential file and its services.",
		run: runInternalBearerKeygen,
	})
}

// How a run draws, reads and writes. A caller changes its own copy; tests replace the random source and set the hook.
type internalBearerKeygenSettings struct {
	// The gate's credential format, whose BearerByteCount sizes the bearer: server.DefaultInternalGateSettings, the
	// values every caller and api read with.
	gateSettings *server.InternalGateSettings
	// The most bytes read of a file that stands at --out or --verify: 64 KiB, far past a verify file holding an entry
	// of every class. A larger file is no file of the gate's, and is refused.
	maxFileByteCount int64
	// The mode both files are written with: their owner alone may read them, as vault's files are.
	outputFileMode os.FileMode
	// Where the bearer's bytes come from: crypto/rand.
	random io.Reader
	// Called before each file is put in place, the verify file first; nil in a run. Tests use it to change a path
	// between the read and the write.
	beforePlace func(file *internalBearerKeygenFile)
}

// The settings of a run.
func defaultInternalBearerKeygenSettings() *internalBearerKeygenSettings {
	return &internalBearerKeygenSettings{
		gateSettings:     server.DefaultInternalGateSettings(),
		maxFileByteCount: 64 * 1024,
		outputFileMode:   0o600,
		random:           rand.Reader,
	}
}

// The classes internal_gate.go knows, in its order, joined by separator.
func internalBearerKeygenClassText(separator string) string {
	classNames := []string{}
	for _, class := range server.InternalGateCallers() {
		classNames = append(classNames, string(class))
	}
	return strings.Join(classNames, separator)
}

// One flag's value, which may be given once.
type internalBearerKeygenFlag struct {
	// the flag, for messages
	flagName string
	value    string
	given    bool
}

// Takes the flag's value, refusing a second, which would leave the run's file a matter of order.
func (self *internalBearerKeygenFlag) set(value string) error {
	if self.given {
		return errors.New("given twice")
	}
	self.value = value
	self.given = true
	return nil
}

// One file a run reads and writes.
type internalBearerKeygenFile struct {
	// the flag that named it, for messages
	flagName string
	path     string
	// whether a file stood at path when the run read it, and its bytes and info; a run replaces that file
	existed bool
	before  []byte
	info    os.FileInfo
	// what the run puts at path
	data []byte
}

// One caller's credential file as internal_gate.go reads it.
type internalBearerKeygenCredentialYaml struct {
	Current  string `yaml:"current"`
	Previous string `yaml:"previous,omitempty"`
}

// One caller's entry in api's verify file as internal_gate.go reads it.
type internalBearerKeygenVerifyEntryYaml struct {
	Class  string   `yaml:"class"`
	Sha256 []string `yaml:"sha256"`
}

// api's verify file as a run edits it: its one document, whose top mapping holds the entries' key and value nodes in
// order, with their comments, and the entries as api decodes them, by caller name.
type internalBearerKeygenVerifyFile struct {
	document      *yaml.Node
	callerEntries map[string]*internalBearerKeygenVerifyEntryYaml
}

// `sidecarctl internal bearer-keygen`.
func runInternalBearerKeygen(args []string, out io.Writer) error {
	return runInternalBearerKeygenWithSettings(args, defaultInternalBearerKeygenSettings(), out)
}

// Parses the command line, reads both files, draws the bearer, reads the new files back and writes both, or neither.
func runInternalBearerKeygenWithSettings(args []string, settings *internalBearerKeygenSettings, out io.Writer) error {
	classFlag := &internalBearerKeygenFlag{flagName: "--class"}
	outFlag := &internalBearerKeygenFlag{flagName: "--out"}
	verifyFlag := &internalBearerKeygenFlag{flagName: "--verify"}
	flags := flag.NewFlagSet("internal bearer-keygen", flag.ContinueOnError)
	flags.Func("class", "", classFlag.set)
	flags.Func("out", "", outFlag.set)
	flags.Func("verify", "", verifyFlag.set)
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 0 {
		// a value standing apart from its flag lands here, as in -class edge: a value follows its flag's =
		return sidecarctlUsageErrorf("takes flags only, each value after an =, as --class=%s; got %q", server.InternalGateCallerEdge, positionals)
	}
	if classFlag.value == "" {
		return sidecarctlUsageErrorf("--class names no class; it is one of %s", internalBearerKeygenClassText(", "))
	}
	for _, pathFlag := range []*internalBearerKeygenFlag{outFlag, verifyFlag} {
		if pathFlag.value == "" {
			return sidecarctlUsageErrorf("%s names no file", pathFlag.flagName)
		}
		if strings.HasPrefix(pathFlag.value, "-") {
			// most likely a flag taken for the value of one written without its =
			return sidecarctlUsageErrorf("%s=%q looks like a flag; name a file beginning with - as ./%s", pathFlag.flagName, pathFlag.value, pathFlag.value)
		}
	}
	class := server.InternalGateCaller(classFlag.value)
	if !slices.Contains(server.InternalGateCallers(), class) {
		return sidecarctlUsageErrorf("--class=%q is no class server/internal_gate.go knows; it is one of %s", classFlag.value, internalBearerKeygenClassText(", "))
	}
	outAbsPath, err := filepath.Abs(outFlag.value)
	if err != nil {
		return err
	}
	verifyAbsPath, err := filepath.Abs(verifyFlag.value)
	if err != nil {
		return err
	}
	if outAbsPath == verifyAbsPath {
		return sidecarctlUsageErrorf("--out and --verify name one file, and the credential file and api's verify file are two")
	}
	// a file named by another file's vault name would be placed in vault as that file
	if filepath.Base(outAbsPath) == server.InternalGateVerifyFileName {
		return sidecarctlUsageErrorf("--out=%s is named as api's verify file is in vault; name class %s's credential file, %s", outFlag.value, class, server.InternalGateCredentialFileName(class))
	}
	for _, otherClass := range server.InternalGateCallers() {
		fileName := server.InternalGateCredentialFileName(otherClass)
		if otherClass != class && filepath.Base(outAbsPath) == fileName {
			return sidecarctlUsageErrorf("--out=%s is named as class %s's credential file is in vault; name class %s's, %s", outFlag.value, otherClass, class, server.InternalGateCredentialFileName(class))
		}
		if filepath.Base(verifyAbsPath) == fileName {
			return sidecarctlUsageErrorf("--verify=%s is named as class %s's credential file is in vault; name api's verify file, %s", verifyFlag.value, otherClass, server.InternalGateVerifyFileName)
		}
	}

	credential := &internalBearerKeygenFile{flagName: "--out", path: outFlag.value}
	verify := &internalBearerKeygenFile{flagName: "--verify", path: verifyFlag.value}
	for _, file := range []*internalBearerKeygenFile{credential, verify} {
		if err := settings.read(file); err != nil {
			return err
		}
	}
	if credential.existed && verify.existed && os.SameFile(credential.info, verify.info) {
		return fmt.Errorf("nothing was written: --out=%s and --verify=%s are one file, and the credential file and api's verify file are two", credential.path, verify.path)
	}

	oldCurrent := ""
	droppedPrevious := false
	if credential.existed {
		if oldCurrent, droppedPrevious, err = parseInternalBearerKeygenCredential(credential); err != nil {
			return err
		}
	}
	var oldVerify *internalBearerKeygenVerifyFile
	if verify.existed {
		if oldVerify, err = parseInternalBearerKeygenVerify(verify); err != nil {
			return err
		}
	} else {
		oldVerify = &internalBearerKeygenVerifyFile{
			document: &yaml.Node{
				Kind:    yaml.DocumentNode,
				Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}},
			},
			callerEntries: map[string]*internalBearerKeygenVerifyEntryYaml{},
		}
	}
	callerName, err := oldVerify.pair(class, credential, verify, oldCurrent)
	if err != nil {
		return err
	}

	bearer, err := settings.draw(oldCurrent)
	if err != nil {
		return err
	}
	credential.data, err = yaml.Marshal(&internalBearerKeygenCredentialYaml{Current: bearer, Previous: oldCurrent})
	if err != nil {
		return fmt.Errorf("nothing was written: --out: %w", err)
	}
	digests := []string{internalBearerKeygenDigest(bearer)}
	if oldCurrent != "" {
		digests = append(digests, internalBearerKeygenDigest(oldCurrent))
	}
	wantCallerEntries := map[string]*internalBearerKeygenVerifyEntryYaml{}
	for otherCallerName, entry := range oldVerify.callerEntries {
		wantCallerEntries[otherCallerName] = entry
	}
	wantCallerEntries[callerName] = &internalBearerKeygenVerifyEntryYaml{Class: string(class), Sha256: digests}
	verify.data, err = oldVerify.withEntry(callerName, internalBearerKeygenVerifyEntryNode(class, digests))
	if err != nil {
		return fmt.Errorf("nothing was written: --verify: %w", err)
	}
	if err := checkInternalBearerKeygenFiles(class, bearer, oldCurrent, credential.data, verify.data, wantCallerEntries); err != nil {
		return err
	}
	if err := settings.write([]*internalBearerKeygenFile{verify, credential}); err != nil {
		return err
	}

	otherClasses := []string{}
	for otherCallerName, entry := range wantCallerEntries {
		if otherCallerName != callerName && !slices.Contains(otherClasses, entry.Class) {
			otherClasses = append(otherClasses, entry.Class)
		}
	}
	slices.Sort(otherClasses)
	fmt.Fprintf(out, "drew a new bearer for class %s: %d random bits, printed nowhere\n", class, 8*settings.gateSettings.BearerByteCount)
	if oldCurrent == "" {
		fmt.Fprintf(out, "wrote --out=%s, readable by its owner alone: class %s's first credential file, the new bearer its current\n", credential.path, class)
	} else {
		dropped := ""
		if droppedPrevious {
			dropped = "; the bearer that was previous is dropped"
		}
		fmt.Fprintf(out, "wrote --out=%s, readable by its owner alone: class %s's credential file, the new bearer its current and the one it replaces its previous%s\n", credential.path, class, dropped)
	}
	takes := "the new bearer"
	if oldCurrent != "" {
		takes = "the new bearer and the one it replaces"
	}
	others := "it holds no other class's entry, so api takes no other class's bearer until each class's entry is added, by this command on this file"
	if 0 < len(otherClasses) {
		others = "its other entries, of classes " + strings.Join(otherClasses, ", ") + ", read as they did"
	}
	fmt.Fprintf(out, "wrote --verify=%s, readable by its owner alone: api's verify file, whose entry %s, of class %s, holds the digests of %s; %s\n", verify.path, callerName, class, takes, others)
	fmt.Fprintf(out, "place %s in vault as %s and deploy api first: it reads the file once at start, and then takes %s\n", verify.path, server.InternalGateVerifyFileName, takes)
	fmt.Fprintf(out, "then place %s in vault as %s and deploy the services that mount it: each reads it once at start and sends the new bearer\n", credential.path, server.InternalGateCredentialFileName(class))
	fmt.Fprintf(out, "a rotation is this command again, --out and --verify naming the files vault then holds: the new bearer becomes current, this one previous, and the one before is dropped from both files\n")
	return nil
}

// Reads the file at the flag's path, when one stands there. Only a regular file is read: a symlink is refused, since
// putting the new file in place would replace the link and leave the file it names as it was, and so is a file
// larger than maxFileByteCount, which is no file of the gate's.
func (self *internalBearerKeygenSettings) read(file *internalBearerKeygenFile) error {
	info, err := os.Lstat(file.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("nothing was written: %s: %w", file.flagName, err)
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("nothing was written: %s=%s is a symlink; name the file itself", file.flagName, file.path)
	case !info.Mode().IsRegular():
		return fmt.Errorf("nothing was written: %s=%s is not a regular file", file.flagName, file.path)
	}
	opened, err := os.Open(file.path)
	if err != nil {
		return fmt.Errorf("nothing was written: %s: %w", file.flagName, err)
	}
	defer opened.Close()
	data, err := io.ReadAll(io.LimitReader(opened, self.maxFileByteCount+1))
	if err != nil {
		return fmt.Errorf("nothing was written: %s: %w", file.flagName, err)
	}
	if self.maxFileByteCount < int64(len(data)) {
		return fmt.Errorf("nothing was written: %s=%s holds more than the %d bytes a file of the gate's may", file.flagName, file.path, self.maxFileByteCount)
	}
	file.existed = true
	file.before = data
	file.info = info
	return nil
}

// The current bearer of the credential file at --out, as the services that mount it read it, and whether the file
// holds a previous one. A file internal_gate.go refuses is refused here, and a rotation never starts over from a
// file that lost its bearer. No message quotes the file: one that does not parse is said not to, without the YAML
// parser's words, which can quote a bearer.
func parseInternalBearerKeygenCredential(file *internalBearerKeygenFile) (string, bool, error) {
	credential := &internalBearerKeygenCredentialYaml{}
	decoder := yaml.NewDecoder(bytes.NewReader(file.before))
	decoder.KnownFields(true)
	if err := decoder.Decode(credential); err != nil {
		return "", false, fmt.Errorf("nothing was written: %s=%s does not parse as a credential file, YAML holding current and, during a rotation, previous; for a class's first file name a path where none stands", file.flagName, file.path)
	}
	// past the YAML, internal_gate.go's refusals name the part alone
	current, err := server.ParseInternalGateCredential(file.before)
	if err != nil {
		return "", false, fmt.Errorf("nothing was written: %s=%s: %w", file.flagName, file.path, err)
	}
	return current, credential.Previous != "", nil
}

// api's verify file at --verify as api reads it, and as a tree a run can edit with its other entries' order and
// comments kept. A file api's verifier refuses is refused, and so is one an edit could not keep faithfully: more than
// one YAML document, of which api reads the first alone, or anchors and aliases, which could leave one entry naming
// another's digests. As with the credential file, no message quotes the YAML parser.
func parseInternalBearerKeygenVerify(file *internalBearerKeygenFile) (*internalBearerKeygenVerifyFile, error) {
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("nothing was written: %s=%s %s", file.flagName, file.path, fmt.Sprintf(format, args...))
	}
	callerEntries := map[string]*internalBearerKeygenVerifyEntryYaml{}
	decoder := yaml.NewDecoder(bytes.NewReader(file.before))
	decoder.KnownFields(true)
	if err := decoder.Decode(&callerEntries); err != nil {
		return nil, refuse("does not parse as api's verify file, a map of caller names each holding its class and its bearers' sha256 digests")
	}
	// past the YAML, the verifier's refusals name a caller and the part, never a digest
	if _, err := server.NewInternalGateVerifier(file.before); err != nil {
		return nil, refuse("is refused by api's verifier: %v", err)
	}
	documents := []*yaml.Node{}
	nodeDecoder := yaml.NewDecoder(bytes.NewReader(file.before))
	for {
		document := &yaml.Node{}
		err := nodeDecoder.Decode(document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, refuse("does not parse as YAML documents")
		}
		documents = append(documents, document)
	}
	if len(documents) != 1 {
		return nil, refuse("holds %d YAML documents, and api reads the first alone; keep that one", len(documents))
	}
	document := documents[0]
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, refuse("is not one map of caller names")
	}
	var holdsAlias func(node *yaml.Node) bool
	holdsAlias = func(node *yaml.Node) bool {
		return node.Kind == yaml.AliasNode || node.Anchor != "" || slices.ContainsFunc(node.Content, holdsAlias)
	}
	if holdsAlias(document) {
		return nil, refuse("uses YAML anchors or aliases, which this does not edit; write each entry out in full")
	}
	return &internalBearerKeygenVerifyFile{document: document, callerEntries: callerEntries}, nil
}

// The caller name the class's entry is under, once the verify file and the credential file are found to be a pair:
// the name of the file's entry of the class, or the class's own name for its first. See the file header for the rules;
// a refusal names the files, the class and its entry, never a digest.
func (self *internalBearerKeygenVerifyFile) pair(class server.InternalGateCaller, credential *internalBearerKeygenFile, verify *internalBearerKeygenFile, oldCurrent string) (string, error) {
	classCallerNames := []string{}
	for callerName, entry := range self.callerEntries {
		if entry.Class == string(class) {
			classCallerNames = append(classCallerNames, callerName)
		}
	}
	slices.Sort(classCallerNames)
	if 1 < len(classCallerNames) {
		return "", fmt.Errorf("nothing was written: --verify=%s holds entries %s, all of class %s, where a class has one credential file and so one entry; merge them by hand", verify.path, strings.Join(classCallerNames, ", "), class)
	}
	if !credential.existed {
		if len(classCallerNames) == 1 {
			return "", fmt.Errorf("nothing was written: --verify=%s holds entry %s of class %s, so the class has a bearer, but no file stands at --out=%s: name the class's credential file from the same vault with --out to rotate it; if no service ever held that bearer, a first draw cut short, remove the entry by hand first", verify.path, classCallerNames[0], class, credential.path)
		}
		if entry, ok := self.callerEntries[string(class)]; ok {
			return "", fmt.Errorf("nothing was written: --verify=%s holds an entry named %s of class %s, so class %s's own entry cannot take that name; rename it by hand", verify.path, class, entry.Class, class)
		}
		return string(class), nil
	}
	if len(classCallerNames) == 0 {
		if !verify.existed {
			return "", fmt.Errorf("nothing was written: --out=%s stands, so this is a rotation, but no file stands at --verify=%s: name api's verify file from the vault --out came from", credential.path, verify.path)
		}
		return "", fmt.Errorf("nothing was written: --out=%s stands, so this is a rotation, but --verify=%s holds no entry of class %s: name api's verify file from the vault --out came from", credential.path, verify.path, class)
	}
	oldCurrentDigest := sha256.Sum256([]byte(oldCurrent))
	holdsOldCurrent := func(entry *internalBearerKeygenVerifyEntryYaml) bool {
		return slices.ContainsFunc(entry.Sha256, func(text string) bool {
			digest, err := hex.DecodeString(text)
			return err == nil && bytes.Equal(digest, oldCurrentDigest[:])
		})
	}
	callerName := classCallerNames[0]
	if !holdsOldCurrent(self.callerEntries[callerName]) {
		return "", fmt.Errorf("nothing was written: --verify=%s's entry %s of class %s does not hold the digest of the bearer --out=%s sends, so the two are no pair: name both files from one vault", verify.path, callerName, class, credential.path)
	}
	for otherCallerName, entry := range self.callerEntries {
		if otherCallerName != callerName && holdsOldCurrent(entry) {
			return "", fmt.Errorf("nothing was written: --verify=%s's entry %s of class %s also holds the digest of the bearer --out=%s sends, so api cannot tell the two apart: give each class its own bearer", verify.path, otherCallerName, entry.Class, credential.path)
		}
	}
	return callerName, nil
}

// The verify file's bytes with entryNode as the value of the entry under callerName, which is added at the end when
// the file holds none. Every other node stays as it was, and the replaced value's comments are kept on the new one.
func (self *internalBearerKeygenVerifyFile) withEntry(callerName string, entryNode *yaml.Node) ([]byte, error) {
	mapping := self.document.Content[0]
	placed := false
	// the decoder refuses a name given twice, so one entry at most is under it
	for index := 0; index+1 < len(mapping.Content) && !placed; index += 2 {
		if mapping.Content[index].Value != callerName {
			continue
		}
		oldNode := mapping.Content[index+1]
		entryNode.HeadComment = oldNode.HeadComment
		entryNode.LineComment = oldNode.LineComment
		entryNode.FootComment = oldNode.FootComment
		mapping.Content[index+1] = entryNode
		placed = true
	}
	if !placed {
		mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: callerName}, entryNode)
	}
	buffer := &bytes.Buffer{}
	encoder := yaml.NewEncoder(buffer)
	if err := encoder.Encode(self.document); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// An entry's value as the doc's example writes it, `{class: <class>, sha256: [<hex>, <hex>]}`. Each scalar is tagged
// a string, so the encoder quotes one that would otherwise read back as a number, as a digest of digits alone would.
func internalBearerKeygenVerifyEntryNode(class server.InternalGateCaller, digests []string) *yaml.Node {
	digestsNode := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, digest := range digests {
		digestsNode.Content = append(digestsNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: digest})
	}
	return &yaml.Node{
		Kind:  yaml.MappingNode,
		Tag:   "!!map",
		Style: yaml.FlowStyle,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "class"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: string(class)},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "sha256"},
			digestsNode,
		},
	}
}

// A bearer's digest as api's verify file holds it: SHA-256 in lowercase hex.
func internalBearerKeygenDigest(bearer string) string {
	digest := sha256.Sum256([]byte(bearer))
	return hex.EncodeToString(digest[:])
}

// Draws the new bearer: BearerByteCount bytes from the settings' random source, in internal_gate.go's encoding,
// unpadded base64url. A source that fails, or gives all zeros or the bearer --out already sends, gives no bearer:
// crypto/rand does neither, so either means the source is broken.
func (self *internalBearerKeygenSettings) draw(oldCurrent string) (string, error) {
	raw := make([]byte, self.gateSettings.BearerByteCount)
	if _, err := io.ReadFull(self.random, raw); err != nil {
		return "", fmt.Errorf("nothing was written: no bearer was drawn: %w", err)
	}
	if !slices.ContainsFunc(raw, func(value byte) bool {
		return value != 0
	}) {
		return "", errors.New("nothing was written: the random source gave all zeros, so it is broken")
	}
	bearer := base64.RawURLEncoding.EncodeToString(raw)
	if bearer == oldCurrent {
		return "", errors.New("nothing was written: the random source gave the bearer --out already sends, so it is broken")
	}
	return bearer, nil
}

// Reads the two new files back as the services and api will, before either is written: the credential file sends the
// new bearer and keeps oldCurrent as previous, api's verifier takes the new bearer, and oldCurrent in a rotation, as
// the class's, and the verify file's entries are wantCallerEntries exactly, the class's holding the new digests and
// every other as it read before the run. No message names a bearer or a digest.
func checkInternalBearerKeygenFiles(class server.InternalGateCaller, bearer string, oldCurrent string, credentialData []byte, verifyData []byte, wantCallerEntries map[string]*internalBearerKeygenVerifyEntryYaml) error {
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("nothing was written: the new files do not read back as internal_gate.go reads them: %s", fmt.Sprintf(format, args...))
	}
	sent, err := server.ParseInternalGateCredential(credentialData)
	if err != nil || sent != bearer {
		return refuse("the credential file does not send the new bearer")
	}
	credential := &internalBearerKeygenCredentialYaml{}
	if err := yaml.Unmarshal(credentialData, credential); err != nil || credential.Previous != oldCurrent {
		return refuse("the credential file does not keep the bearer it replaces as previous")
	}
	verifier, err := server.NewInternalGateVerifier(verifyData)
	if err != nil {
		return refuse("api's verifier refuses the verify file: %v", err)
	}
	bearers := []string{bearer}
	if oldCurrent != "" {
		bearers = append(bearers, oldCurrent)
	}
	for _, sentBearer := range bearers {
		request, err := http.NewRequest(http.MethodGet, server.InternalGatePathPrefix, nil)
		if err != nil {
			return refuse("%v", err)
		}
		request.Header.Set("Authorization", "Bearer "+sentBearer)
		if caller, ok := verifier.Authenticate(request); !ok || caller != class {
			return refuse("api's verifier does not take a bearer of the credential file as class %s's", class)
		}
	}
	callerEntries := map[string]*internalBearerKeygenVerifyEntryYaml{}
	decoder := yaml.NewDecoder(bytes.NewReader(verifyData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&callerEntries); err != nil || !reflect.DeepEqual(callerEntries, wantCallerEntries) {
		return refuse("the verify file's entries are not the ones it held with class %s's set to the new digests", class)
	}
	return nil
}

// Puts each file's new bytes at its path, in order, all of them or none. Each goes into a new file beside its path,
// given outputFileMode and synced, before any is put in place. Then each is linked at its path when no file stood
// there, so a file that appeared meanwhile is never written over, or renamed over the file that stood there. When one
// cannot be put in place, those already placed are put back as the run found them, and the error says whether that
// worked. The directories are synced last, so the names last a crash. No file beside is left behind.
func (self *internalBearerKeygenSettings) write(files []*internalBearerKeygenFile) error {
	besidePaths := []string{}
	defer func() {
		// after a rename this finds nothing, and after a link the path keeps the file
		for _, besidePath := range besidePaths {
			os.Remove(besidePath)
		}
	}()
	for _, file := range files {
		besidePath, err := self.writeBeside(file.path, file.data)
		if besidePath != "" {
			besidePaths = append(besidePaths, besidePath)
		}
		if err != nil {
			return fmt.Errorf("nothing was written: %s: %w", file.flagName, err)
		}
	}
	placedFiles := []*internalBearerKeygenFile{}
	for index, file := range files {
		if self.beforePlace != nil {
			self.beforePlace(file)
		}
		if err := placeInternalBearerKeygenFile(besidePaths[index], file); err != nil {
			for _, placedFile := range placedFiles {
				if putBackErr := self.putBack(placedFile); putBackErr != nil {
					return fmt.Errorf("%w; and %s=%s, already placed, could not be put back as it was (%v), so the two files are no pair: put back that file from vault", err, placedFile.flagName, placedFile.path, putBackErr)
				}
			}
			return fmt.Errorf("nothing was written: %w", err)
		}
		placedFiles = append(placedFiles, file)
	}
	directories := []string{}
	for _, file := range files {
		if directory := filepath.Dir(file.path); !slices.Contains(directories, directory) {
			directories = append(directories, directory)
		}
	}
	for _, directory := range directories {
		directoryFile, err := os.Open(directory)
		if err == nil {
			err = directoryFile.Sync()
			directoryFile.Close()
		}
		if err != nil {
			return fmt.Errorf("wrote both files, but directory %s was not synced, so a crash could still lose them: %w", directory, err)
		}
	}
	return nil
}

// Writes data to a new file beside path, given outputFileMode and synced, and returns the new file's path, even with
// an error once the file exists, so the caller removes it.
func (self *internalBearerKeygenSettings) writeBeside(path string, data []byte) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return "", err
	}
	// a mode set apart from the create, which the umask cannot narrow
	err = file.Chmod(self.outputFileMode)
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return file.Name(), err
}

// Puts the file beside at the file's path: a rename over the file that stood there, or a link when none did, refused
// when one appeared meanwhile.
func placeInternalBearerKeygenFile(besidePath string, file *internalBearerKeygenFile) error {
	if file.existed {
		if err := os.Rename(besidePath, file.path); err != nil {
			return fmt.Errorf("%s: %w", file.flagName, err)
		}
		return nil
	}
	err := os.Link(besidePath, file.path)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s=%s appeared during the run, and this writes over a file only when it stood there when the run read it", file.flagName, file.path)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", file.flagName, err)
	}
	return nil
}

// Puts a placed file's path back as the run found it: removed when no file stood there, or the bytes that stood
// there written beside it and renamed in place.
func (self *internalBearerKeygenSettings) putBack(file *internalBearerKeygenFile) error {
	if !file.existed {
		return os.Remove(file.path)
	}
	besidePath, err := self.writeBeside(file.path, file.before)
	if besidePath != "" {
		defer os.Remove(besidePath)
	}
	if err != nil {
		return err
	}
	return os.Rename(besidePath, file.path)
}
