package fleet

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"strings"
)

// Countries maps IP addresses to ISO 3166 country codes from a range file:
// CSV lines "first_ip,last_ip,CC" (the DB-IP "IP to Country Lite" layout,
// IPv4 and IPv6). Lines whose code is not two letters are skipped.
type Countries struct {
	ranges []ipRange
	codes  map[string]struct{}
}

type ipRange struct {
	first, last netip.Addr
	country     string
}

// LoadCountries reads a range file.
func LoadCountries(path string) (*Countries, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadCountries(f)
}

// ReadCountries parses range lines from r.
func ReadCountries(r io.Reader) (*Countries, error) {
	reader := csv.NewReader(bufio.NewReader(r))
	reader.FieldsPerRecord = -1
	c := &Countries{codes: map[string]struct{}{}}
	for line := 1; ; line++ {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if len(record) < 3 || len(record[2]) != 2 {
			continue
		}
		first, errFirst := netip.ParseAddr(strings.TrimSpace(record[0]))
		last, errLast := netip.ParseAddr(strings.TrimSpace(record[1]))
		if errFirst != nil || errLast != nil || first.Is4() != last.Is4() || last.Less(first) {
			return nil, fmt.Errorf("line %d: invalid range", line)
		}
		code := strings.ToUpper(record[2])
		c.ranges = append(c.ranges, ipRange{first.Unmap(), last.Unmap(), code})
		c.codes[code] = struct{}{}
	}
	sort.Slice(c.ranges, func(i, j int) bool { return c.ranges[i].first.Less(c.ranges[j].first) })
	return c, nil
}

// Lookup returns the country of addr, or "" when no range holds it.
func (c *Countries) Lookup(addr netip.Addr) string {
	if c == nil {
		return ""
	}
	addr = addr.Unmap()
	i := sort.Search(len(c.ranges), func(i int) bool { return addr.Less(c.ranges[i].first) })
	if i == 0 {
		return ""
	}
	r := c.ranges[i-1]
	if r.first.Is4() == addr.Is4() && !r.last.Less(addr) {
		return r.country
	}
	return ""
}

// Has reports whether any range carries the country code.
func (c *Countries) Has(country string) bool {
	_, ok := c.codes[strings.ToUpper(country)]
	return ok
}

// Len is the number of ranges loaded.
func (c *Countries) Len() int { return len(c.ranges) }
