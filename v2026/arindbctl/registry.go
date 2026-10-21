package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The NRO extended delegated statistics identify, per registry, the resource
// holder of every ASN through an opaque id. ASNs sharing a holder are
// registry siblings: likely one operator, never proof of one service. The
// audit uses them to suggest catalog merges and additions; the build does not.
// https://ftp.ripe.net/pub/stats/ripencc/RIR-Statistics-Exchange-Format.txt
type registrySource struct {
	countryEvidenceSource `yaml:",inline" json:",inline"`
	Format                string `yaml:"format" json:"format"`
}

const (
	registryFormatNroDelegatedStats = "nro-delegated-stats"
	registryFormatCaidaAs2org       = "caida-as2org-jsonl"
	maxRegistryDecompressedBytes    = 256 << 20
	maxRegistryAsnRecords           = 1 << 20
)

type registryHolders struct {
	holderByASN  map[uint32]string
	asnsByHolder map[string][]uint32
	caidaByASN   map[uint32]string
	asnsByCaida  map[string][]uint32
	records      int
}

func (self *registryHolders) add(holder string, asn uint32) error {
	if self.holderByASN == nil {
		self.holderByASN, self.asnsByHolder = map[uint32]string{}, map[string][]uint32{}
	}
	if existing, ok := self.holderByASN[asn]; ok && existing != holder {
		return errors.New("registry statistics assign one ASN to two holders")
	}
	if _, ok := self.holderByASN[asn]; ok {
		return nil
	}
	self.holderByASN[asn] = holder
	self.asnsByHolder[holder] = append(self.asnsByHolder[holder], asn)
	self.records++
	if self.records > maxRegistryAsnRecords {
		return errors.New("registry statistics exceed the ASN record bound")
	}
	return nil
}

// Holder ids are only unique within one registry's file, so the registry name
// is part of the key. Only assigned/allocated ASN records name a holder.
func readNroDelegatedStats(ctx context.Context, raw io.Reader, into *registryHolders) error {
	reader, err := openBoundedEvidenceReader(raw, maxRegistryDecompressedBytes)
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 4096)
	version, rows := false, 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "|")
		if !version {
			if len(fields) < 7 || fields[0] != "2" {
				return errors.New("registry statistics lack the version-two header")
			}
			version = true
			continue
		}
		if len(fields) < 7 || fields[2] != "asn" || fields[1] == "*" {
			continue
		}
		if fields[6] != "assigned" && fields[6] != "allocated" {
			continue
		}
		if len(fields) < 8 || strings.TrimSpace(fields[7]) == "" {
			return errors.New("registry statistics ASN record lacks a holder id")
		}
		start, startErr := strconv.ParseUint(fields[3], 10, 32)
		count, countErr := strconv.ParseUint(fields[4], 10, 32)
		if startErr != nil || countErr != nil || count == 0 || start+count-1 > 4294967295 {
			return errors.New("registry statistics ASN record is malformed")
		}
		holder := strings.ToLower(fields[0]) + "/" + strings.TrimSpace(fields[7])
		for asn := start; asn < start+count; asn++ {
			if err := into.add(holder, uint32(asn)); err != nil {
				return err
			}
		}
		rows++
	}
	if err := scanner.Err(); err != nil {
		return errors.New("registry statistics are truncated or unreadable")
	}
	if !version || rows == 0 {
		return errors.New("registry statistics contain no ASN records")
	}
	return nil
}

// CAIDA's AS2Org groups ASNs by WHOIS organization across all five RIRs; its
// organization ids are globally unique, unlike delegated-statistics holders.
// https://publicdata.caida.org/datasets/as-organizations/README.txt
func readCaidaAs2org(ctx context.Context, raw io.Reader, into *registryHolders) error {
	reader, err := openBoundedEvidenceReader(raw, maxRegistryDecompressedBytes)
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	rows := 0
	for scanner.Scan() {
		if rows%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Type           string `json:"type"`
			ASN            string `json:"asn"`
			OrganizationId string `json:"organizationId"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			return errors.New("CAIDA AS2Org line is malformed")
		}
		if entry.Type != "ASN" {
			continue
		}
		asn, err := strconv.ParseUint(entry.ASN, 10, 32)
		if err != nil || asn == 0 || strings.TrimSpace(entry.OrganizationId) == "" {
			return errors.New("CAIDA AS2Org ASN line lacks its ASN or organization")
		}
		if err := into.addCaida("caida/"+entry.OrganizationId, uint32(asn)); err != nil {
			return err
		}
		rows++
	}
	if err := scanner.Err(); err != nil {
		return errors.New("CAIDA AS2Org is truncated or unreadable")
	}
	if rows == 0 {
		return errors.New("CAIDA AS2Org contains no ASN lines")
	}
	return nil
}

// A second grouping beside the registry holder; an ASN may carry both.
func (self *registryHolders) addCaida(holder string, asn uint32) error {
	if self.caidaByASN == nil {
		self.caidaByASN, self.asnsByCaida = map[uint32]string{}, map[string][]uint32{}
	}
	if existing, ok := self.caidaByASN[asn]; ok {
		if existing != holder {
			return errors.New("CAIDA AS2Org assigns one ASN to two organizations")
		}
		return nil
	}
	self.caidaByASN[asn] = holder
	self.asnsByCaida[holder] = append(self.asnsByCaida[holder], asn)
	self.records++
	if self.records > 2*maxRegistryAsnRecords {
		return errors.New("registry groupings exceed their ASN record bound")
	}
	return nil
}

func loadRegistryHolders(ctx context.Context, catalogPath string, sources []registrySource) (*registryHolders, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	root, err := os.OpenRoot(filepath.Dir(catalogPath))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	holders := &registryHolders{}
	for _, source := range sources {
		f, err := root.Open(source.File)
		if err != nil {
			return nil, err
		}
		parseErr := func() error {
			switch source.Format {
			case registryFormatNroDelegatedStats:
				return readNroDelegatedStats(ctx, f, holders)
			case registryFormatCaidaAs2org:
				return readCaidaAs2org(ctx, f, holders)
			default:
				return errors.New("unsupported registry statistics format")
			}
		}()
		closeErr := f.Close()
		if parseErr != nil {
			return nil, parseErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	return holders, nil
}

// Sibling ASNs of a set, excluding the set itself, sorted.
func (self *registryHolders) siblings(asns []uint32) []uint32 {
	if self == nil {
		return nil
	}
	siblings := []uint32{}
	for _, asn := range asns {
		groups := [][]uint32{}
		if holder, ok := self.holderByASN[asn]; ok {
			groups = append(groups, self.asnsByHolder[holder])
		}
		if organization, ok := self.caidaByASN[asn]; ok {
			groups = append(groups, self.asnsByCaida[organization])
		}
		for _, group := range groups {
			for _, sibling := range group {
				if !slices.Contains(asns, sibling) && !slices.Contains(siblings, sibling) {
					siblings = append(siblings, sibling)
				}
			}
		}
	}
	slices.Sort(siblings)
	return siblings
}

func (self *registryHolders) holdersOf(asns []uint32) []string {
	if self == nil {
		return nil
	}
	holders := []string{}
	for _, asn := range asns {
		if holder, ok := self.holderByASN[asn]; ok && !slices.Contains(holders, holder) {
			holders = append(holders, holder)
		}
		if organization, ok := self.caidaByASN[asn]; ok && !slices.Contains(holders, organization) {
			holders = append(holders, organization)
		}
	}
	slices.Sort(holders)
	return holders
}
