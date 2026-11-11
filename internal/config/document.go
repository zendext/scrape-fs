package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2/unstable"
)

type Document struct {
	Text            string            `json:"text"`
	Digest          string            `json:"digest"`
	EffectiveDigest string            `json:"effective_digest"`
	Fields          map[string]string `json:"fields"`
	ValidationError string            `json:"validation_error,omitempty"`
}

func Digest(configuration Config) string {
	data, _ := json.Marshal(configuration)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

func ParseDocument(text string) (Document, error) {
	document := Document{Text: text, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(text)))}
	configuration, err := Decode(strings.NewReader(text))
	if err != nil {
		document.ValidationError = err.Error()
		return document, err
	}
	return Document{
		Text: text, Digest: document.Digest, EffectiveDigest: Digest(configuration),
		Fields: map[string]string{
			"storage.database_path":      configuration.Storage.DatabasePath,
			"storage.overlay_store_path": configuration.Storage.OverlayStorePath,
			"fuse.mountpoint":            configuration.FUSE.Mountpoint,
			"fuse.entry_timeout":         configuration.FUSE.EntryTimeout.String(),
			"fuse.attr_timeout":          configuration.FUSE.AttrTimeout.String(),
			"fuse.negative_timeout":      configuration.FUSE.NegativeTimeout.String(),
			"control.socket_path":        configuration.Control.SocketPath,
			"control.socket_mode":        fmt.Sprintf("0o%o", configuration.Control.SocketMode),
			"control.administrator_uid":  strconv.FormatUint(uint64(configuration.Control.AdministratorUID), 10),
			"gc.orphan_grace_period":     configuration.GC.OrphanGracePeriod.String(),
		},
	}, nil
}

func PatchDocument(text string, fields map[string]string) (Document, error) {
	original, err := ParseDocument(text)
	if err != nil {
		return Document{}, err
	}
	replacements := map[string]string{}
	for key, value := range fields {
		previous, ok := original.Fields[key]
		if !ok {
			return Document{}, fmt.Errorf("unknown configuration field %q", key)
		}
		if value == previous {
			continue
		}
		encoded := strconv.Quote(value)
		if key == "control.socket_mode" || key == "control.administrator_uid" {
			encoded = value
		}
		replacements[key] = encoded
	}
	type edit struct {
		start, end int
		value      string
	}
	edits := []edit{}
	var parser unstable.Parser
	parser.Reset([]byte(text))
	table := ""
	tableEnds := map[string]int{}
	for parser.NextExpression() {
		expression := parser.Expression()
		if expression.Kind != unstable.Table && expression.Kind != unstable.KeyValue {
			continue
		}
		parts := []string{}
		keyOffset := 0
		keys := expression.Key()
		for keys.Next() {
			if len(parts) == 0 {
				keyOffset = int(keys.Node().Raw.Offset)
			}
			parts = append(parts, string(keys.Node().Data))
		}
		key := strings.Join(parts, ".")
		if expression.Kind == unstable.Table {
			tableEnds[table] = strings.LastIndex(text[:keyOffset], "\n") + 1
			table = key
			continue
		}
		if table != "" {
			key = table + "." + key
		}
		if value, ok := replacements[key]; ok {
			raw := expression.Value().Raw
			edits = append(edits, edit{int(raw.Offset), int(raw.Offset + raw.Length), value})
			delete(replacements, key)
		}
	}
	if err := parser.Error(); err != nil {
		return Document{}, err
	}
	tableEnds[table] = len(text)
	keys := make([]string, 0, len(replacements))
	for key := range replacements {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	insertions := map[int]string{}
	newTables := map[string]string{}
	for _, key := range keys {
		parts := strings.SplitN(key, ".", 2)
		line := parts[1] + " = " + replacements[key] + "\n"
		if offset, ok := tableEnds[parts[0]]; ok {
			insertions[offset] += "\n" + line
		} else {
			newTables[parts[0]] += line
		}
	}
	sections := make([]string, 0, len(newTables))
	for section := range newTables {
		sections = append(sections, section)
	}
	sort.Strings(sections)
	for _, section := range sections {
		insertions[len(text)] += "\n[" + section + "]\n" + newTables[section]
	}
	for offset, value := range insertions {
		edits = append(edits, edit{offset, offset, value})
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	for _, edit := range edits {
		text = text[:edit.start] + edit.value + text[edit.end:]
	}
	return ParseDocument(text)
}
