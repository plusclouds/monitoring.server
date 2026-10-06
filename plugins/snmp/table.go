package snmp

import (
	"errors"
	"strings"

	"github.com/gosnmp/gosnmp"
)

// maxVarbinds bounds one GETBULK response: rows × columns per request.
const maxVarbinds = 60

// table reads several columns of a table at once with GETBULK (one request
// carries a cursor per column), or column by column on SNMPv1. It returns
// rows by index (the OID suffix after the column), each holding the values
// by column position. Missing cells are absent.
func (s *session) table(cols []string) (map[string]map[int]gosnmp.SnmpPDU, error) {
	rows := map[string]map[int]gosnmp.SnmpPDU{}
	put := func(col int, p gosnmp.SnmpPDU) bool {
		prefix := cols[col] + "."
		if !strings.HasPrefix(p.Name, prefix) || !present(p) {
			return false
		}
		idx := p.Name[len(prefix):]
		if rows[idx] == nil {
			rows[idx] = map[int]gosnmp.SnmpPDU{}
		}
		rows[idx][col] = p
		return true
	}
	if s.Version == gosnmp.Version1 {
		for i, c := range cols {
			pdus, err := s.walk(c)
			if err != nil {
				return nil, err
			}
			for _, p := range pdus {
				put(i, p)
			}
		}
		return rows, nil
	}

	cursor := append([]string(nil), cols...)
	active := make([]int, len(cols)) // column positions still being read
	for i := range active {
		active[i] = i
	}
	for round := 0; len(active) > 0; round++ {
		if round > 10000 {
			return nil, errors.New("table walk did not finish: the agent keeps returning rows")
		}
		oids := make([]string, len(active))
		for i, c := range active {
			oids[i] = cursor[c]
		}
		reps := uint32(max(maxVarbinds/len(active), 1)) //nolint:gosec // small positive
		pkt, err := s.GetBulk(oids, 0, reps)
		if err != nil {
			return nil, err
		}
		if pkt.Error != gosnmp.NoError {
			return nil, errors.New("agent returned " + pkt.Error.String())
		}
		done := map[int]bool{}
		// Varbinds come row by row: one per active column, repeated.
		for i, v := range pkt.Variables {
			c := active[i%len(active)]
			if done[c] {
				continue
			}
			v.Name = normalizeOID(v.Name)
			if !put(c, v) || !oidAfter(v.Name, cursor[c]) {
				done[c] = true
				continue
			}
			cursor[c] = v.Name
		}
		if len(pkt.Variables) == 0 {
			break
		}
		next := active[:0]
		for _, c := range active {
			if !done[c] {
				next = append(next, c)
			}
		}
		active = next
	}
	return rows, nil
}

// oidAfter reports whether a comes after b in OID order, so a broken agent
// that repeats itself cannot loop forever.
func oidAfter(a, b string) bool {
	pa, pb := strings.Split(strings.Trim(a, "."), "."), strings.Split(strings.Trim(b, "."), ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] == pb[i] {
			continue
		}
		if len(pa[i]) != len(pb[i]) {
			return len(pa[i]) > len(pb[i])
		}
		return pa[i] > pb[i]
	}
	return len(pa) > len(pb)
}
