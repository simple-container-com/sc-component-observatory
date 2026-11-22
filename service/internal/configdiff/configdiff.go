// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package configdiff answers "what changed since the previous deploy"
// from two stored client-config snapshots.
//
// Deliberately generic: it knows nothing about AWS, Yandex Cloud,
// Kubernetes, ECS or a compose file. It flattens both snapshots to
// dotted paths, diffs the leaves, and sorts each change into a bucket
// by a path-segment rule. A new deploy target therefore needs no change
// here — the same property the healthcheck has, and for the same
// reason: what is cloud-specific is DATA on the record, not code.
//
// Diffing at READ time over a stored snapshot is the other deliberate
// choice. The AWS-only ancestor (`simple-container-com/observatory`)
// stored only the `previous_*` fields its differ produced, so improving
// the differ could never improve an existing timeline — every past
// deploy stayed frozen at the comparison the old code happened to make,
// and a field nobody thought to extract was gone for good. Keeping the
// snapshot and computing on read means a better rule applies to all of
// history.
package configdiff

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Change kinds.
const (
	KindAdded   = "added"
	KindRemoved = "removed"
	KindChanged = "changed"
)

// Bounds. Every one of these exists because this runs in a Lambda with
// a response-size cap, over a config an operator controls the size of.
const (
	// maxDepth collapses anything deeper into a single JSON blob leaf,
	// so a deeply nested change still shows up as changed rather than
	// being walked forever.
	maxDepth = 12

	// maxLeaves bounds the flatten of ONE snapshot.
	maxLeaves = 4000

	// maxChanges bounds the rendered diff. A deploy that changed more
	// than this is better described as "replaced" than enumerated.
	maxChanges = 250

	// maxValueLen truncates a single displayed value.
	maxValueLen = 200
)

// redactedPrefix is what the plugin's sanitizer writes in place of a
// credential. It carries a keyed fingerprint suffix
// (`[redacted:ab12cd]`) precisely so a ROTATED secret diffs as changed
// instead of as unchanged, which is what a constant marker did.
const redactedPrefix = "[redacted"

// Change is one leaf that differs between two snapshots.
type Change struct {
	// Path is the full dotted path into the snapshot, kept for
	// precision — two buckets can hold the same display Key.
	Path string `json:"path"`

	// Key is the display label within its bucket (the wrapper segments
	// and the bucket's own segment stripped).
	Key string `json:"key"`

	Kind string `json:"kind"`

	Previous string `json:"previous,omitempty"`
	Current  string `json:"current,omitempty"`

	// Secret marks a value that was redacted at the source. The two
	// sides are shown as `[redacted]` — the fingerprints proved they
	// differ, which is the whole signal, and printing them would add
	// noise without adding information.
	Secret bool `json:"secret,omitempty"`
}

// Diff is the rendered answer for one deploy.
//
// Every slice is non-nil so the SPA can `.map()` unguarded — the first
// SC Component's blank-screen failure mode was one absent array.
type Diff struct {
	// Env is the consumer's environment variables: the bucket an
	// operator checks first, and the one most likely to explain a
	// behaviour change.
	Env []Change `json:"env"`

	// Compute is the deployed shape — CPU, memory, scaling bounds,
	// timeouts. Named generically because "replicas" on k8s, "desired
	// count" on ECS and "provisioned concurrency" on Lambda are the
	// same question.
	Compute []Change `json:"compute"`

	// Settings is everything else structural: the template, domains,
	// `uses:`, schedules, healthchecks.
	Settings []Change `json:"settings"`

	// Artifact is the fields that change on EVERY deploy by
	// construction — the image tag, the version. Bucketed rather than
	// dropped: hiding them would be a small lie, and an unexpected
	// image NAME change (a different repository) is worth seeing.
	Artifact []Change `json:"artifact"`

	// Unavailable states WHY no comparison could be made. Load-bearing:
	// an absent snapshot must never render as "no changes". That
	// conflation is the silent degradation this whole component exists
	// to avoid.
	Unavailable string `json:"unavailable,omitempty"`

	// Truncated says the diff hit a bound above.
	Truncated bool `json:"truncated,omitempty"`
}

// Count is the number of rendered changes.
func (d Diff) Count() int {
	return len(d.Env) + len(d.Compute) + len(d.Settings) + len(d.Artifact)
}

// Empty reports a computed diff that found nothing — distinct from one
// that could not be computed at all.
func (d Diff) Empty() bool { return d.Unavailable == "" && d.Count() == 0 }

// Unavailable builds a diff that explains itself instead of showing an
// empty one.
func Unavailable(reason string) Diff {
	d := empty()
	d.Unavailable = reason
	return d
}

func empty() Diff {
	return Diff{
		Env:      []Change{},
		Compute:  []Change{},
		Settings: []Change{},
		Artifact: []Change{},
	}
}

// Compute diffs two snapshots, previous → current.
//
// A missing snapshot on either side yields Unavailable, never an empty
// diff: "we did not record it" and "nothing changed" are different
// answers and an operator acts differently on each.
func Compute(previous, current map[string]any) Diff {
	switch {
	case len(previous) == 0 && len(current) == 0:
		return Unavailable("neither this deploy nor the one before it recorded a config snapshot")
	case len(previous) == 0:
		return Unavailable("the previous deploy recorded no config snapshot, so there is nothing to compare against")
	case len(current) == 0:
		return Unavailable("this deploy recorded no config snapshot")
	}

	prev, prevTrunc := flatten(previous)
	cur, curTrunc := flatten(current)

	out := empty()
	out.Truncated = prevTrunc || curTrunc

	paths := make([]string, 0, len(cur)+len(prev))
	seen := make(map[string]bool, len(cur)+len(prev))
	for p := range prev {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for p := range cur {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	for _, p := range paths {
		pv, inPrev := prev[p]
		cv, inCur := cur[p]
		if inPrev && inCur && pv == cv {
			continue
		}
		if out.Count() >= maxChanges {
			out.Truncated = true
			break
		}

		bucket, key := classify(p)
		ch := Change{Path: p, Key: key}
		switch {
		case !inPrev:
			ch.Kind = KindAdded
		case !inCur:
			ch.Kind = KindRemoved
		default:
			ch.Kind = KindChanged
		}
		if isRedacted(pv) || isRedacted(cv) {
			ch.Secret = true
			if inPrev {
				pv = "[redacted]"
			}
			if inCur {
				cv = "[redacted]"
			}
		}
		// Truncation is a DISPLAY step, applied after the comparison
		// above. Truncating inside flatten would make two long values
		// that differ only past the cap compare equal, and the change
		// would vanish with no Truncated flag to hint at it.
		ch.Previous, ch.Current = truncate(pv), truncate(cv)

		switch bucket {
		case bucketEnv:
			out.Env = append(out.Env, ch)
		case bucketCompute:
			out.Compute = append(out.Compute, ch)
		case bucketArtifact:
			out.Artifact = append(out.Artifact, ch)
		default:
			out.Settings = append(out.Settings, ch)
		}
	}
	return out
}

func isRedacted(v string) bool { return strings.HasPrefix(v, redactedPrefix) }

// flatten turns a snapshot into leaf path → displayable value.
func flatten(root map[string]any) (map[string]string, bool) {
	out := make(map[string]string, 64)
	truncated := false
	walk(out, "", root, 0, &truncated)
	return out, truncated
}

func walk(out map[string]string, prefix string, v any, depth int, truncated *bool) {
	if len(out) >= maxLeaves {
		*truncated = true
		return
	}
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			out[prefix] = "{}"
			return
		}
		if depth >= maxDepth {
			// Collapse rather than recurse forever: a change below the
			// cap still renders as a changed blob.
			out[prefix] = blob(t)
			*truncated = true
			return
		}
		for k, child := range t {
			walk(out, join(prefix, k), child, depth+1, truncated)
		}
	case []any:
		if len(t) == 0 {
			out[prefix] = "[]"
			return
		}
		// A list of scalars is one value to a reader — `uses: [a, b]`
		// reads as a set, not as two independent leaves whose indexes
		// shift when one is removed.
		if allScalar(t) {
			out[prefix] = blob(t)
			return
		}
		if depth >= maxDepth {
			out[prefix] = blob(t)
			*truncated = true
			return
		}
		for i, item := range t {
			walk(out, fmt.Sprintf("%s[%d]", prefix, i), item, depth+1, truncated)
		}
	default:
		out[prefix] = scalar(v)
	}
}

func allScalar(items []any) bool {
	for _, item := range items {
		switch item.(type) {
		case map[string]any, []any:
			return false
		}
	}
	return true
}

func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// scalar and blob keep the FULL value: they feed the comparison, and
// Compute truncates for display once it knows the leaf actually
// changed.
func scalar(v any) string {
	if v == nil {
		return "null"
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func blob(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(raw)
}

// truncate is rune-safe: cutting mid-codepoint would put invalid UTF-8
// into a JSON response, and encoding/json replaces it silently.
func truncate(s string) string {
	if len(s) <= maxValueLen {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxValueLen {
		return s
	}
	return string(runes[:maxValueLen]) + "…"
}

// Buckets.
const (
	bucketEnv      = "env"
	bucketCompute  = "compute"
	bucketSettings = "settings"
	bucketArtifact = "artifact"
)

// envSegments mark the sub-tree that holds environment variables.
var envSegments = map[string]bool{
	"env": true, "environment": true, "envvariables": true, "environmentvariables": true,
}

// computeSegments mark a sub-tree that describes the deployed shape.
// Matched anywhere in the path, so `scale.min` and
// `resources.limits.cpu` both land in Compute without listing every
// leaf a cloud might invent.
// `size` and `scale` are what the fleet's own client.yaml files
// actually use (cloud-compose/ECS writes `size: {cpu, memory}` and
// `scale: {min, max}`), which is also why `scale.min` must classify
// from the segment rather than from the leaf name `min`.
var computeSegments = map[string]bool{
	"scale": true, "scalepolicy": true, "autoscaling": true, "size": true,
	"resources": true, "limits": true, "requests": true, "compute": true,
}

// computeLeaves are the leaf names that mean "deployed shape" on their
// own, across clouds — Lambda's memory/timeout, ECS's desired count,
// k8s's replicas, YC's instance bounds.
//
// `maxMemory` is not a guess: it is the single most common compute knob
// in the fleet's client.yaml files (19 occurrences across the forge-*
// repos, against zero for `memorySize`). The AWS-flavoured spellings
// are kept anyway — they cost nothing and a misfiled change is still
// shown, just under Settings.
var computeLeaves = map[string]bool{
	"cpu": true, "memory": true, "maxmemory": true, "memorysize": true, "memorymb": true,
	"minscale": true, "maxscale": true, "replicas": true, "instances": true,
	"instancecount": true, "desiredcount": true, "concurrency": true,
	"maxconcurrency": true, "provisionedconcurrency": true, "timeout": true,
	"ephemeralstorage": true, "disksize": true, "gpu": true, "cores": true,
}

// artifactLeaves change on every deploy by construction.
var artifactLeaves = map[string]bool{
	"image": true, "imagetag": true, "tag": true, "digest": true,
	"version": true, "deployversion": true, "buildid": true,
	"sha": true, "commit": true,
}

// classify sorts one path into a bucket and derives its display key.
//
// Segment-based rather than a list of known fields, because the point
// is to work for a template this code has never seen. The buckets are
// presentational: a misfiled change is still SHOWN, with its full path,
// which is why erring toward Settings is safe.
func classify(path string) (bucket, key string) {
	segs := strings.Split(path, ".")
	display := displaySegments(segs)

	for i, s := range display {
		if envSegments[normalize(s)] && i < len(display)-1 {
			return bucketEnv, strings.Join(display[i+1:], ".")
		}
	}

	leaf := normalize(display[len(display)-1])
	// Strip a trailing index so `images[0]` classifies like `image`.
	if artifactLeaves[leaf] {
		return bucketArtifact, strings.Join(display, ".")
	}
	if computeLeaves[leaf] {
		return bucketCompute, strings.Join(display, ".")
	}
	for _, s := range display {
		if computeSegments[normalize(s)] {
			return bucketCompute, strings.Join(display, ".")
		}
	}
	return bucketSettings, strings.Join(display, ".")
}

// displaySegments strips the descriptor's wrapper levels.
//
// What the plugin captures is `stack.Client.Stacks[env]`, which arrives
// as {"Config": {"config": {…}}} — a capitalized embedded Go field with
// no json tag, wrapping a lowercase one. Showing an operator
// `Config.config.env.FOO` when they wrote `FOO` is noise, so leading
// `config` segments are dropped. Done by NAME rather than by position
// so an api change that adds or removes a level does not start leaking
// wrapper names into the UI.
func displaySegments(segs []string) []string {
	i := 0
	for i < len(segs)-1 && normalize(stripIndex(segs[i])) == "config" {
		i++
	}
	return segs[i:]
}

func stripIndex(s string) string {
	if i := strings.Index(s, "["); i > 0 {
		return s[:i]
	}
	return s
}

// normalize lowercases and drops separators, so `min_scale`,
// `minScale` and `MinScale` are one name.
func normalize(s string) string {
	s = stripIndex(s)
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r == '_' || r == '-' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
