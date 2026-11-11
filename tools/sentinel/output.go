package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
)

// printJSON pretty-prints raw JSON to w (two-space indent, matching what an operator or another
// agent piping this into `jq`/a log would expect).
func printJSON(w io.Writer, raw json.RawMessage) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		// Not valid JSON (shouldn't happen for a well-formed server) — fall back to raw bytes
		// rather than swallowing the response.
		_, werr := w.Write(raw)
		fmt.Fprintln(w)
		return werr
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

// printTable renders a slice of flat JSON objects (found at listKey inside raw, or raw itself if
// listKey is "") as a text/tabwriter table. Column order is the union of keys across all rows,
// sorted for determinism. Used for the --format table variant of list-shaped commands.
func printTable(w io.Writer, raw json.RawMessage, listKey string) error {
	var container map[string]json.RawMessage
	var listRaw json.RawMessage

	if listKey == "" {
		listRaw = raw
	} else {
		if err := json.Unmarshal(raw, &container); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
		lr, ok := container[listKey]
		if !ok {
			return fmt.Errorf("response has no %q field to tabulate", listKey)
		}
		listRaw = lr
	}

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(listRaw, &rows); err != nil {
		return fmt.Errorf("decoding %q as a list: %w", listKey, err)
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "(no rows)")
		return nil
	}

	colSet := map[string]bool{}
	for _, row := range rows {
		for k := range row {
			colSet[k] = true
		}
	}
	cols := make([]string, 0, len(colSet))
	for k := range colSet {
		cols = append(cols, k)
	}
	sort.Strings(cols)

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for i, c := range cols {
		if i > 0 {
			fmt.Fprint(tw, "\t")
		}
		fmt.Fprint(tw, c)
	}
	fmt.Fprintln(tw)

	for _, row := range rows {
		for i, c := range cols {
			if i > 0 {
				fmt.Fprint(tw, "\t")
			}
			fmt.Fprint(tw, cellString(row[c]))
		}
		fmt.Fprintln(tw)
	}
	return tw.Flush()
}

// cellString renders one JSON value compactly enough for a table cell: strings unquoted,
// everything else (numbers, bools, nested objects/arrays, null) as compact JSON text.
func cellString(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// output writes raw to w in the requested format. format must be "json" (default) or "table";
// table is only meaningful for list-shaped responses and needs listKey to find the array (pass ""
// when raw itself is the array).
func output(w io.Writer, format string, raw json.RawMessage, listKey string) error {
	switch format {
	case "", "json":
		return printJSON(w, raw)
	case "table":
		return printTable(w, raw, listKey)
	default:
		return fmt.Errorf("unknown -format %q: must be \"json\" or \"table\"", format)
	}
}
