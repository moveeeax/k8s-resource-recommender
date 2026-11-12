// Package manifest writes recommendations back into Deployment manifests in a
// Git checkout, so right-sizing arrives as a reviewable diff.
//
// Files are edited as text, not re-encoded. yaml.v3 parses each document to
// find the nodes and their line/column positions, and only the scalar values
// under spec.template.spec.containers[].resources are spliced in place, or new
// lines are inserted next to them. Everything else in the file, including
// comments, key order, quoting and the indentation style of sequences, stays
// byte for byte, so the diff shows only the resource changes.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/moveeeax/k8s-resource-recommender/internal/recommend"
)

// Options controls the rewrite.
type Options struct {
	// Namespace is assumed for manifests without metadata.namespace, the
	// usual layout when kustomize or Helm sets it per directory. Empty means
	// such manifests are skipped, because guessing would edit the wrong file.
	Namespace string
	// MinChange leaves a container alone when both its CPU and memory request
	// would move by less than this fraction, e.g. 0.1 for 10%.
	MinChange float64
	// OmitCPULimit removes an existing CPU limit instead of setting one.
	OmitCPULimit bool
}

// Change is one container whose requests were rewritten.
type Change struct {
	File      string // relative to the directory passed to Rewrite
	Namespace string
	Workload  string
	Container string
	// Old values are as written in the manifest, "-" when absent.
	OldCPU, NewCPU string
	OldMem, NewMem string
}

// Result lists what Rewrite did and what it could not do.
type Result struct {
	Changes []Change
	// Warnings are files, documents or containers that were skipped.
	Warnings []string
	// Unmatched are namespace/name workloads with a recommendation but no
	// Deployment manifest in the directory.
	Unmatched []string
}

// Rewrite walks dir for .yaml and .yml files and updates every Deployment
// that has a recommendation. Hidden directories such as .git are skipped.
func Rewrite(dir string, recs []recommend.Recommendation, o Options) (Result, error) {
	byWorkload := map[string]map[string]recommend.Recommendation{}
	for _, r := range recs {
		k := recommend.WorkloadKey(r.Namespace, r.Deployment)
		if byWorkload[k] == nil {
			byWorkload[k] = map[string]recommend.Recommendation{}
		}
		byWorkload[k][r.Container] = r
	}

	var res Result
	matched := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			rel = path
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out, changes, warnings := rewriteFile(src, rel, byWorkload, matched, o)
		res.Warnings = append(res.Warnings, warnings...)
		if len(changes) == 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, out, info.Mode().Perm()); err != nil {
			return err
		}
		res.Changes = append(res.Changes, changes...)
		return nil
	})
	if err != nil {
		return res, err
	}

	for k := range byWorkload {
		if !matched[k] {
			ns, name, _ := strings.Cut(k, "\x00")
			res.Unmatched = append(res.Unmatched, ns+"/"+name)
		}
	}
	sort.Strings(res.Unmatched)
	return res, nil
}

// rewriteFile returns the edited source and the changes made. A file that is
// not valid YAML (a Helm template, say) is skipped with a warning.
func rewriteFile(src []byte, file string, recs map[string]map[string]recommend.Recommendation,
	matched map[string]bool, o Options) ([]byte, []Change, []string) {
	newline := "\n"
	if bytes.Contains(src, []byte("\r\n")) {
		newline = "\r\n"
	}
	lines := strings.Split(string(src), "\n")
	if newline == "\r\n" {
		for i := range lines {
			lines[i] = strings.TrimSuffix(lines[i], "\r")
		}
	}

	var (
		edits    []edit
		changes  []Change
		warnings []string
	)
	dec := yaml.NewDecoder(bytes.NewReader(src))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return src, nil, []string{fmt.Sprintf("%s: skipped, not valid YAML: %v", file, err)}
		}
		if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
			continue
		}
		root := doc.Content[0]
		if scalar(root, "kind") != "Deployment" {
			continue
		}
		meta := get(root, "metadata")
		name, ns := scalar(meta, "name"), scalar(meta, "namespace")
		if name == "" {
			continue
		}
		if ns == "" {
			if o.Namespace == "" {
				warnings = append(warnings, fmt.Sprintf("%s: Deployment %s has no metadata.namespace; pass --namespace to match it", file, name))
				continue
			}
			ns = o.Namespace
		}
		key := recommend.WorkloadKey(ns, name)
		byContainer := recs[key]
		if byContainer == nil {
			continue
		}
		matched[key] = true

		containers := get(get(get(get(root, "spec"), "template"), "spec"), "containers")
		if containers == nil || containers.Kind != yaml.SequenceNode {
			warnings = append(warnings, fmt.Sprintf("%s: %s/%s has no spec.template.spec.containers list", file, ns, name))
			continue
		}
		seen := map[string]bool{}
		for _, c := range containers.Content {
			cname := scalar(c, "name")
			r, ok := byContainer[cname]
			if !ok {
				continue
			}
			seen[cname] = true
			e, ch, err := planContainer(lines, c, r, o)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%s: %s/%s container %s skipped: %v", file, ns, name, cname, err))
				continue
			}
			if ch == nil {
				continue
			}
			ch.File, ch.Namespace, ch.Workload, ch.Container = file, ns, name, cname
			edits = append(edits, e...)
			changes = append(changes, *ch)
		}
		for cname := range byContainer {
			if !seen[cname] {
				warnings = append(warnings, fmt.Sprintf("%s: %s/%s has no container %s in the manifest (injected sidecar?)", file, ns, name, cname))
			}
		}
	}
	if len(edits) == 0 {
		return src, nil, warnings
	}
	return []byte(strings.Join(apply(lines, edits), newline)), changes, warnings
}

// indentStep is the indentation used for lines this package inserts.
const indentStep = 2

// planContainer computes the edits for one container mapping. It returns a
// nil Change when the container needs no edit.
func planContainer(lines []string, c *yaml.Node, r recommend.Recommendation, o Options) ([]edit, *Change, error) {
	requests := []kv{{"cpu", r.CPURequest.String()}, {"memory", r.MemRequest.String()}}
	limits := []kv{{"memory", r.MemLimit.String()}}
	if r.CPULimit != nil && !o.OmitCPULimit {
		limits = []kv{{"cpu", r.CPULimit.String()}, {"memory", r.MemLimit.String()}}
	}

	resKey, res := getKV(c, "resources")
	ch := &Change{OldCPU: "-", OldMem: "-", NewCPU: requests[0].v, NewMem: requests[1].v}
	if res != nil && res.Kind == yaml.MappingNode {
		if req := get(res, "requests"); req != nil {
			if v := get(req, "cpu"); v != nil && v.Kind == yaml.ScalarNode {
				ch.OldCPU = v.Value
			}
			if v := get(req, "memory"); v != nil && v.Kind == yaml.ScalarNode {
				ch.OldMem = v.Value
			}
		}
	}
	if o.MinChange > 0 && small(ch.OldCPU, r.CPURequest, o.MinChange) && small(ch.OldMem, r.MemRequest, o.MinChange) {
		return nil, nil, nil
	}

	var edits []edit
	switch {
	case res == nil:
		// No resources key at all: add the whole block right after the
		// container's name, which is always a one-line scalar.
		nameKey, nameVal := getKV(c, "name")
		if nameVal == nil || nameVal.Kind != yaml.ScalarNode || nameVal.Line != nameKey.Line {
			return nil, nil, fmt.Errorf("cannot find a one-line name to insert resources after")
		}
		edits = append(edits, insertAfter(nameVal.Line, resourcesBlock(nameKey.Column-1, requests, limits)))
	case isEmpty(res):
		e, err := clearEmpty(lines, resKey, res)
		if err != nil {
			return nil, nil, err
		}
		edits = append(edits, e...)
		indent := resKey.Column - 1 + indentStep
		block := append(section(indent, "requests", requests), section(indent, "limits", limits)...)
		edits = append(edits, insertAfter(resKey.Line, block))
	case res.Kind == yaml.MappingNode && res.Style&yaml.FlowStyle == 0:
		var missing []string
		indent := res.Content[0].Column - 1
		for _, s := range []struct {
			name string
			vals []kv
			drop bool
		}{
			{"requests", requests, false},
			{"limits", limits, o.OmitCPULimit},
		} {
			secKey, sec := getKV(res, s.name)
			if sec == nil {
				missing = append(missing, section(indent, s.name, s.vals)...)
				continue
			}
			e, err := planSection(lines, secKey, sec, s.vals, s.drop)
			if err != nil {
				return nil, nil, fmt.Errorf("resources.%s: %w", s.name, err)
			}
			edits = append(edits, e...)
		}
		if len(missing) > 0 {
			edits = append(edits, insertBefore(res.Content[0].Line, missing))
		}
	default:
		return nil, nil, fmt.Errorf("resources is not a block mapping; edit it by hand")
	}

	if len(edits) == 0 {
		return nil, nil, nil
	}
	return edits, ch, nil
}

// planSection updates one requests/limits mapping. drop removes an existing
// cpu entry instead of keeping it.
func planSection(lines []string, secKey, sec *yaml.Node, vals []kv, drop bool) ([]edit, error) {
	if isEmpty(sec) {
		e, err := clearEmpty(lines, secKey, sec)
		if err != nil {
			return nil, err
		}
		return append(e, insertAfter(secKey.Line, entries(secKey.Column-1+indentStep, vals))), nil
	}
	if sec.Kind != yaml.MappingNode || sec.Style&yaml.FlowStyle != 0 {
		return nil, fmt.Errorf("not a block mapping; edit it by hand")
	}

	var edits []edit
	var missing []kv
	for _, want := range vals {
		_, v := getKV(sec, want.k)
		if v == nil {
			missing = append(missing, want)
			continue
		}
		if same(v.Value, want.v) {
			continue
		}
		e, err := replaceScalar(lines, v, want.v)
		if err != nil {
			return nil, err
		}
		edits = append(edits, e)
	}
	if drop {
		if k, v := getKV(sec, "cpu"); v != nil {
			if v.Kind != yaml.ScalarNode || v.Line != k.Line {
				return nil, fmt.Errorf("cpu is not a one-line value")
			}
			edits = append(edits, edit{line: k.Line, op: opDelete})
		}
	}
	if len(missing) > 0 {
		edits = append(edits, insertBefore(sec.Content[0].Line, entries(sec.Content[0].Column-1, missing)))
	}
	return edits, nil
}

type kv struct{ k, v string }

func resourcesBlock(indent int, requests, limits []kv) []string {
	out := []string{pad(indent) + "resources:"}
	out = append(out, section(indent+indentStep, "requests", requests)...)
	return append(out, section(indent+indentStep, "limits", limits)...)
}

func section(indent int, name string, vals []kv) []string {
	return append([]string{pad(indent) + name + ":"}, entries(indent+indentStep, vals)...)
}

func entries(indent int, vals []kv) []string {
	out := make([]string, 0, len(vals))
	for _, e := range vals {
		out = append(out, pad(indent)+e.k+": "+e.v)
	}
	return out
}

func pad(n int) string { return strings.Repeat(" ", n) }

// isEmpty reports a value written as nothing, null, ~ or {}.
func isEmpty(n *yaml.Node) bool {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Tag == "!!null"
	case yaml.MappingNode:
		return len(n.Content) == 0
	}
	return false
}

// clearEmpty removes the null or {} token after a key so a block can follow.
// A bare "key:" has no token and needs no edit.
func clearEmpty(lines []string, key, val *yaml.Node) ([]edit, error) {
	tok := ""
	switch {
	case val.Kind == yaml.MappingNode:
		tok = "{}"
	case val.Value != "":
		tok = val.Value
	}
	if tok == "" {
		return nil, nil
	}
	if val.Line != key.Line {
		return nil, fmt.Errorf("%s value is not on the key's line", key.Value)
	}
	col := val.Column - 1
	line := []rune(lines[val.Line-1])
	if col > len(line) || !strings.HasPrefix(string(line[col:]), tok) {
		return nil, fmt.Errorf("unexpected text at line %d", val.Line)
	}
	// Take the separating space with it, so "resources: {}" becomes
	// "resources:" rather than "resources: ".
	if col > 0 && line[col-1] == ' ' {
		col--
		tok = " " + tok
	}
	return []edit{{line: val.Line, op: opReplace, col: col, old: tok}}, nil
}

// replaceScalar swaps a one-line scalar in place, keeping its quote style.
func replaceScalar(lines []string, v *yaml.Node, value string) (edit, error) {
	if v.Kind != yaml.ScalarNode {
		return edit{}, fmt.Errorf("line %d: value is not a scalar", v.Line)
	}
	tok, repl := v.Value, value
	switch {
	case v.Style&yaml.DoubleQuotedStyle != 0:
		tok, repl = `"`+v.Value+`"`, `"`+value+`"`
	case v.Style&yaml.SingleQuotedStyle != 0:
		tok, repl = "'"+v.Value+"'", "'"+value+"'"
	case v.Style != 0:
		return edit{}, fmt.Errorf("line %d: unsupported scalar style", v.Line)
	}
	col := v.Column - 1
	line := []rune(lines[v.Line-1])
	if col > len(line) || !strings.HasPrefix(string(line[col:]), tok) {
		return edit{}, fmt.Errorf("line %d: value is not written as %s", v.Line, tok)
	}
	return edit{line: v.Line, op: opReplace, col: col, old: tok, new: repl}, nil
}

// same compares two quantities by value, so "0.25" and "250m" are equal.
func same(old, want string) bool {
	a, err := resource.ParseQuantity(old)
	if err != nil {
		return false
	}
	b, err := resource.ParseQuantity(want)
	if err != nil {
		return false
	}
	return a.Cmp(b) == 0
}

// small reports whether moving from old to want is below the threshold. A
// missing or unparsable old value always counts as a real change.
func small(old string, want resource.Quantity, threshold float64) bool {
	q, err := resource.ParseQuantity(old)
	if err != nil {
		return false
	}
	o, w := q.AsApproximateFloat64(), want.AsApproximateFloat64()
	if o == 0 {
		return w == 0
	}
	return math.Abs(w-o)/o < threshold
}

// get returns the value under key in a mapping node, or nil.
func get(m *yaml.Node, key string) *yaml.Node {
	_, v := getKV(m, key)
	return v
}

func getKV(m *yaml.Node, key string) (k, v *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

func scalar(m *yaml.Node, key string) string {
	if v := get(m, key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}
	return ""
}

type op int

// Ops on the same line are applied in this order, so that a replace sees the
// line before anything is inserted around it.
const (
	opReplace op = iota
	opDelete
	opInsertAfter
	opInsertBefore
)

// edit is one text operation on a 1-based line.
type edit struct {
	line  int
	op    op
	col   int    // opReplace: 0-based rune offset of old
	old   string // opReplace: text expected at col
	new   string // opReplace: replacement
	lines []string
}

func insertAfter(line int, lines []string) edit {
	return edit{line: line, op: opInsertAfter, lines: lines}
}

func insertBefore(line int, lines []string) edit {
	return edit{line: line, op: opInsertBefore, lines: lines}
}

// apply runs the edits bottom-up, so earlier line numbers stay valid.
func apply(lines []string, edits []edit) []string {
	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].line != edits[j].line {
			return edits[i].line > edits[j].line
		}
		return edits[i].op < edits[j].op
	})
	for _, e := range edits {
		i := e.line - 1
		switch e.op {
		case opReplace:
			r := []rune(lines[i])
			lines[i] = string(r[:e.col]) + e.new + string(r[e.col+len([]rune(e.old)):])
		case opDelete:
			lines = append(lines[:i], lines[i+1:]...)
		case opInsertAfter:
			lines = insertAt(lines, i+1, e.lines)
		case opInsertBefore:
			lines = insertAt(lines, i, e.lines)
		}
	}
	return lines
}

func insertAt(lines []string, i int, add []string) []string {
	out := make([]string, 0, len(lines)+len(add))
	out = append(out, lines[:i]...)
	out = append(out, add...)
	return append(out, lines[i:]...)
}
