// Package outputs keeps a cell's outputs as files (ADR 1792457255 §4.6.2): each output of a run, or
// of an .ipynb code cell, is the next file NN.<ext> in the cell's folder, <note>.lab/out/<key>/,
// where AutoDoc's export and Rendered view find them (lab.Outputs).
//
//	stream (stdout, stderr), consecutive ones of a name joined   NN.txt
//	error: its traceback, ANSI colours stripped                   NN.txt
//	application/vnd.dataresource+json (a pandas DataFrame)        NN.csv, and a Table for host.table
//	image/png, image/jpeg                                          NN.png, NN.jpg
//	image/svg+xml                                                  NN.svg
//	application/vnd.vegalite.v5+json (or v4)                      NN.chart.json
//	anything else                                                  its text/plain, as NN.txt
package outputs

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// File is one output kept: its extension and its bytes.
type File struct {
	Ext  string
	Data []byte
}

// Bundle is a display_data's or execute_result's data: each MIME type's value, a string, a list of
// strings (as .ipynb keeps text), or JSON.
type Bundle map[string]json.RawMessage

// Table is a DataFrame for the plugin's table panel (protocol 4): its columns, its rows' cells as
// text, and each row's kinds (plugin.KindText, …).
type Table struct {
	Fields []string
	Rows   [][]string
	Kinds  []string
}

// The bundle's MIME types the files are kept from, in the order one is chosen.
const (
	MIMEDataResource = "application/vnd.dataresource+json"
	MIMEPNG          = "image/png"
	MIMEJPEG         = "image/jpeg"
	MIMESVG          = "image/svg+xml"
	MIMEVegaLite5    = "application/vnd.vegalite.v5+json"
	MIMEVegaLite4    = "application/vnd.vegalite.v4+json"
	MIMEText         = "text/plain"
)

// FromBundle is the file a bundle is kept as, the first of its types the table above names, with the
// Table of a DataFrame; false when it holds none of them.
func FromBundle(b Bundle) (File, *Table, bool) {
	if raw, ok := b[MIMEDataResource]; ok {
		if f, t, ok := dataResource(raw); ok {
			return f, t, true
		}
	}
	for _, img := range []struct{ mime, ext string }{{MIMEPNG, ".png"}, {MIMEJPEG, ".jpg"}} {
		if raw, ok := b[img.mime]; ok {
			data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(text(raw)), ""))
			if err == nil {
				return File{Ext: img.ext, Data: data}, nil, true
			}
		}
	}
	if raw, ok := b[MIMESVG]; ok {
		return File{Ext: ".svg", Data: []byte(text(raw))}, nil, true
	}
	for _, mime := range []string{MIMEVegaLite5, MIMEVegaLite4} {
		if raw, ok := b[mime]; ok {
			var spec any
			if json.Unmarshal(raw, &spec) == nil {
				if data, err := json.MarshalIndent(spec, "", "  "); err == nil {
					return File{Ext: ".chart.json", Data: append(data, '\n')}, nil, true
				}
			}
		}
	}
	if raw, ok := b[MIMEText]; ok {
		return File{Ext: ".txt", Data: []byte(text(raw))}, nil, true
	}
	return File{}, nil, false
}

// text is a bundle value as text: a string, or the lines of a list joined, as .ipynb keeps them.
func text(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var lines []string
	if json.Unmarshal(raw, &lines) == nil {
		return strings.Join(lines, "")
	}
	return string(raw)
}

// ansi is an ANSI escape sequence: a traceback's colours.
var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// Error is an error's file: its traceback, colours stripped; "ename: evalue" when it has none.
func Error(ename, evalue string, traceback []string) File {
	s := strings.Join(traceback, "\n")
	if strings.TrimSpace(s) == "" {
		s = ename + ": " + evalue
	}
	return File{Ext: ".txt", Data: []byte(ansi.ReplaceAllString(s, "") + "\n")}
}

// dataResource is a DataFrame's table schema as a CSV file and a Table: its fields in the schema's
// order (the index first, as pandas sends it), a null cell empty in the CSV and "null" in the table.
func dataResource(raw json.RawMessage) (File, *Table, bool) {
	var dr struct {
		Schema struct {
			Fields []struct {
				Name string `json:"name"`
			} `json:"fields"`
		} `json:"schema"`
		Data []map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &dr) != nil || len(dr.Schema.Fields) == 0 {
		return File{}, nil, false
	}
	t := &Table{}
	for _, f := range dr.Schema.Fields {
		t.Fields = append(t.Fields, f.Name)
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write(t.Fields)
	for _, row := range dr.Data {
		file, shown := make([]string, len(t.Fields)), make([]string, len(t.Fields))
		kinds := make([]byte, len(t.Fields))
		for i, f := range t.Fields {
			file[i], shown[i], kinds[i] = cell(row[f])
		}
		_ = w.Write(file)
		t.Rows = append(t.Rows, shown)
		t.Kinds = append(t.Kinds, string(kinds))
	}
	w.Flush()
	return File{Ext: ".csv", Data: buf.Bytes()}, t, true
}

// cell is a table-schema value as the CSV keeps it and as the table shows it, with its kind: the
// protocol's kind bytes ('t' text, 'z' null, 'n' number, 'b' bool, 'j' JSON).
func cell(raw json.RawMessage) (file, shown string, kind byte) {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0 || string(raw) == "null":
		return "", "null", 'z'
	case string(raw) == "true" || string(raw) == "false":
		return string(raw), string(raw), 'b'
	case raw[0] == '"':
		var s string
		_ = json.Unmarshal(raw, &s)
		return s, s, 't'
	case raw[0] == '{' || raw[0] == '[':
		return string(raw), string(raw), 'j'
	default:
		if _, err := strconv.ParseFloat(string(raw), 64); err == nil {
			return string(raw), string(raw), 'n'
		}
		return string(raw), string(raw), 't'
	}
}

// Run gathers one run's outputs from its messages, in order.
type Run struct {
	files      []File
	streamName string // the stream being joined; "" when the last output was not a stream
	// Tables are the run's DataFrames, in order.
	Tables []Table
}

// Add takes one output message of the run: stream, display_data, execute_result, error or
// clear_output (which drops what the run kept so far); others are not outputs.
func (r *Run) Add(msgType string, content json.RawMessage) {
	switch msgType {
	case "stream":
		var s struct{ Name, Text string }
		if json.Unmarshal(content, &s) != nil {
			return
		}
		if s.Name == r.streamName && len(r.files) > 0 {
			last := &r.files[len(r.files)-1]
			last.Data = append(last.Data, s.Text...)
			return
		}
		r.files = append(r.files, File{Ext: ".txt", Data: []byte(s.Text)})
		r.streamName = s.Name
		return
	case "display_data", "execute_result":
		var d struct {
			Data Bundle `json:"data"`
		}
		if json.Unmarshal(content, &d) != nil {
			return
		}
		if f, t, ok := FromBundle(d.Data); ok {
			r.files = append(r.files, f)
			if t != nil {
				r.Tables = append(r.Tables, *t)
			}
		}
	case "error":
		var e struct {
			EName     string   `json:"ename"`
			EValue    string   `json:"evalue"`
			Traceback []string `json:"traceback"`
		}
		if json.Unmarshal(content, &e) != nil {
			return
		}
		r.files = append(r.files, Error(e.EName, e.EValue, e.Traceback))
	case "clear_output":
		r.files, r.Tables = nil, nil
	default:
		return
	}
	r.streamName = ""
}

// Files are the run's outputs, in order.
func (r *Run) Files() []File { return slices.Clone(r.files) }

// Commit makes files the cell folder dir's whole content, NN.<ext> in order: written to a hidden
// folder beside it, then swapped in, so a reader sees the older run's outputs or this one's, never
// a mix, and none of an older run's extra files survives. No files remove the folder.
func Commit(dir string, files []File) error {
	parent, base := filepath.Split(filepath.Clean(dir))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, "."+base+".run-")
	if err != nil {
		return err
	}
	width := max(2, len(strconv.Itoa(len(files))))
	for i, f := range files {
		name := fmt.Sprintf("%0*d%s", width, i+1, f.Ext)
		if err := os.WriteFile(filepath.Join(tmp, name), f.Data, 0o644); err != nil {
			os.RemoveAll(tmp)
			return err
		}
	}
	old := ""
	if _, err := os.Stat(dir); err == nil {
		old = filepath.Join(parent, "."+base+".old-"+filepath.Base(tmp)[len(base)+6:])
		if err := os.Rename(dir, old); err != nil {
			os.RemoveAll(tmp)
			return err
		}
	}
	if len(files) == 0 {
		os.RemoveAll(tmp)
	} else if err := os.Rename(tmp, dir); err != nil {
		if old != "" {
			_ = os.Rename(old, dir) // the older run's outputs back
		}
		os.RemoveAll(tmp)
		return err
	}
	if old != "" {
		return os.RemoveAll(old)
	}
	return nil
}

// Prune removes the cell folders under outDir whose key is not in keep: outputs of code no cell has
// any more, after a Run all. Hidden entries (a run being written) are left.
func Prune(outDir string, keep []string) error {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || slices.Contains(keep, e.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(outDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
