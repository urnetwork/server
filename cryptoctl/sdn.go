package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/internal/cryptosanctionslist"
)

// What update keeps of an SDN.XML publication.
type sdnPublication struct {
	// OFAC's Publish_Date, as YYYY-MM-DD
	publishDate string
	recordCount int
	entries     int
	ids         []digitalCurrencyId
}

// One "Digital Currency Address - <code>" identifier of an SDN entry.
type digitalCurrencyId struct {
	// OFAC's code, as XBT; "" if the identifier names none
	currency string
	// as listed, without surrounding white space
	address string
	sdnUid  int
	sdnName string
}

type sdnEntry struct {
	Uid       int     `xml:"uid"`
	FirstName string  `xml:"firstName"`
	LastName  string  `xml:"lastName"`
	Ids       []sdnId `xml:"idList>id"`
}

type sdnId struct {
	IdType   string `xml:"idType"`
	IdNumber string `xml:"idNumber"`
}

const digitalCurrencyIdType = "Digital Currency Address"

// The code of a Digital Currency Address identifier type, as "XBT" of
// "Digital Currency Address - XBT".
func digitalCurrencyCode(idType string) (string, bool) {
	idType = strings.TrimSpace(idType)
	if len(idType) < len(digitalCurrencyIdType) || !strings.EqualFold(idType[:len(digitalCurrencyIdType)], digitalCurrencyIdType) {
		return "", false
	}
	code := strings.TrimSpace(idType[len(digitalCurrencyIdType):])
	code = strings.TrimSpace(strings.TrimPrefix(code, "-"))
	return strings.ToUpper(code), true
}

// Reads an SDN.XML document whole. It must be an sdnList with a publish date
// and a record count equal to its entries, so a truncated or partial download
// is refused, and it must carry Digital Currency Address identifiers.
func parseSdnList(reader io.Reader) (*sdnPublication, error) {
	decoder := xml.NewDecoder(reader)
	publication := &sdnPublication{}
	root := false
	closed := false
	published := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("unreadable SDN XML: %w", err)
		}
		// children are decoded whole, so the only end seen here is the root's
		if end, ok := token.(xml.EndElement); ok && root && end.Name.Local == "sdnList" {
			closed = true
			continue
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if closed {
			return nil, fmt.Errorf("element %q after sdnList", start.Name.Local)
		}
		if !root {
			if start.Name.Local != "sdnList" {
				return nil, fmt.Errorf("root element %q, want sdnList", start.Name.Local)
			}
			root = true
			continue
		}
		switch start.Name.Local {
		case "publshInformation":
			var information struct {
				PublishDate string `xml:"Publish_Date"`
				RecordCount string `xml:"Record_Count"`
			}
			if err := decoder.DecodeElement(&information, &start); err != nil {
				return nil, fmt.Errorf("unreadable publshInformation: %w", err)
			}
			date, err := time.Parse("01/02/2006", strings.TrimSpace(information.PublishDate))
			if err != nil {
				return nil, fmt.Errorf("publish date %q: %w", information.PublishDate, err)
			}
			count, err := strconv.Atoi(strings.TrimSpace(information.RecordCount))
			if err != nil {
				return nil, fmt.Errorf("record count %q: %w", information.RecordCount, err)
			}
			publication.publishDate = date.Format("2006-01-02")
			publication.recordCount = count
			published = true
		case "sdnEntry":
			var entry sdnEntry
			if err := decoder.DecodeElement(&entry, &start); err != nil {
				return nil, fmt.Errorf("unreadable sdnEntry: %w", err)
			}
			publication.entries += 1
			name := strings.TrimSpace(entry.LastName)
			if first := strings.TrimSpace(entry.FirstName); first != "" {
				name += ", " + first
			}
			for _, id := range entry.Ids {
				currency, ok := digitalCurrencyCode(id.IdType)
				if !ok {
					continue
				}
				publication.ids = append(publication.ids, digitalCurrencyId{
					currency: currency,
					address:  strings.TrimSpace(id.IdNumber),
					sdnUid:   entry.Uid,
					sdnName:  name,
				})
			}
		default:
			if err := decoder.Skip(); err != nil {
				return nil, fmt.Errorf("unreadable %s: %w", start.Name.Local, err)
			}
		}
	}
	if !root {
		return nil, errors.New("no sdnList")
	}
	if !closed {
		return nil, errors.New("sdnList is not closed: the document is truncated")
	}
	if !published {
		return nil, errors.New("no publshInformation")
	}
	if publication.recordCount != publication.entries {
		return nil, fmt.Errorf("%d entries, but the record count is %d", publication.entries, publication.recordCount)
	}
	if len(publication.ids) == 0 {
		return nil, fmt.Errorf("no %s identifier in %d entries", digitalCurrencyIdType, publication.entries)
	}
	return publication, nil
}

// The table of a publication, ready for cryptosanctionslist.WriteTable.
type classifiedTable struct {
	entries         map[string][]cryptosanctionslist.Entry
	ofacCurrencyIds map[string]int
	unrecognized    []cryptosanctionslist.Unrecognized
}

// Files every identifier under each asset whose chain accepts it, merging the
// codes and SDN entries that list one canonical address; reports the
// identifiers under an asset's own code that its chain does not accept.
func classify(publication *sdnPublication) classifiedTable {
	type listing struct {
		currencies map[string]bool
		sdn        map[int]string
	}
	listings := map[string]map[string]*listing{}
	unrecognized := map[cryptosanctionslist.Unrecognized]bool{}
	table := classifiedTable{
		entries:         map[string][]cryptosanctionslist.Entry{},
		ofacCurrencyIds: map[string]int{},
	}
	for _, id := range publication.ids {
		table.ofacCurrencyIds[id.currency] += 1
		for _, asset := range cryptosanctionslist.Assets() {
			canonical, err := cryptosanctionslist.Normalize(asset, id.address)
			if err != nil {
				if own, _ := cryptosanctionslist.OfacCurrency(asset); own == id.currency {
					unrecognized[cryptosanctionslist.Unrecognized{
						Asset:        asset,
						OfacCurrency: id.currency,
						Address:      id.address,
						SdnUid:       id.sdnUid,
						Reason:       err.Error(),
					}] = true
				}
				continue
			}
			if listings[asset] == nil {
				listings[asset] = map[string]*listing{}
			}
			found := listings[asset][canonical]
			if found == nil {
				found = &listing{currencies: map[string]bool{}, sdn: map[int]string{}}
				listings[asset][canonical] = found
			}
			found.currencies[id.currency] = true
			found.sdn[id.sdnUid] = id.sdnName
		}
	}
	for _, asset := range cryptosanctionslist.Assets() {
		entries := []cryptosanctionslist.Entry{}
		for address, found := range listings[asset] {
			entry := cryptosanctionslist.Entry{Address: address, OfacCurrencies: []string{}, Sdn: []cryptosanctionslist.SdnRef{}}
			for currency := range found.currencies {
				entry.OfacCurrencies = append(entry.OfacCurrencies, currency)
			}
			slices.Sort(entry.OfacCurrencies)
			for uid, name := range found.sdn {
				entry.Sdn = append(entry.Sdn, cryptosanctionslist.SdnRef{Uid: uid, Name: name})
			}
			slices.SortFunc(entry.Sdn, func(a cryptosanctionslist.SdnRef, b cryptosanctionslist.SdnRef) int { return a.Uid - b.Uid })
			entries = append(entries, entry)
		}
		slices.SortFunc(entries, func(a cryptosanctionslist.Entry, b cryptosanctionslist.Entry) int {
			return strings.Compare(a.Address, b.Address)
		})
		table.entries[asset] = entries
	}
	assetOrder := map[string]int{}
	for i, asset := range cryptosanctionslist.Assets() {
		assetOrder[asset] = i
	}
	for item := range unrecognized {
		table.unrecognized = append(table.unrecognized, item)
	}
	slices.SortFunc(table.unrecognized, func(a cryptosanctionslist.Unrecognized, b cryptosanctionslist.Unrecognized) int {
		if order := assetOrder[a.Asset] - assetOrder[b.Asset]; order != 0 {
			return order
		}
		if order := strings.Compare(a.Address, b.Address); order != 0 {
			return order
		}
		return a.SdnUid - b.SdnUid
	})
	return table
}
