# Label-Based Watch & LIST Indexing for Kubernetes CRDs

**Branch**: `crusoe-label-index-watch-dispatch`
**Base**: Kubernetes v1.36.0
**Diff**: +156/-31 lines across 13 files (8 Go source files + 5 test files)

## Problem

The Kubernetes API server's watch cache has two performance bottlenecks for Custom Resources at scale:

1. **WATCH dispatch is O(n)**: When a CRD object is patched, the cacher scans ALL watchers to find which ones care about the event. At 60K agents watching with label selectors, this means 60K evaluations per event — 99.4% wasted.

2. **LIST filtering is O(n)**: When an agent does an initial LIST with a label selector, the watch cache scans ALL objects of that CRD type to find matches. At 60K agents starting up, this creates a "LIST storm" of 1.8M requests each scanning 300K+ objects.

Built-in types like Pods avoid both problems using `TriggerFunc` (indexed WATCH dispatch via `spec.nodeName`) and `cache.Indexers` (indexed LIST filtering). **CRDs have neither.**

## Solution

A CRD annotation `crusoe.ai/watch-index-label` enables label-based indexing for both WATCH dispatch and LIST filtering. When set, the API server:

1. Registers watchers using label selectors with `in(...)` into indexed buckets (`valueWatchers`) instead of the unindexed `allWatchers` map
2. Indexes objects in the watch cache by the specified label key for O(1) LIST lookups

### Activation

Add the annotation to any CRD that should have label-based indexing:

```yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: sdnvpcnics.sdn.crusoe.ai
  annotations:
    crusoe.ai/watch-index-label: "vpc-id"   # <-- enables indexing
spec:
  # ... rest of CRD unchanged
```

**CRDs without the annotation are completely unaffected** — all new code paths are gated on `watchIndexLabel != ""`.

## How It Works

### Without annotation (stock behavior)

```
WATCH: Agent watches with labelSelector=vpc-id in (42,43,...,51)
  → Watcher goes into allWatchers map
  → Every event scans ALL watchers → O(60K) per event

LIST: Agent LISTs with labelSelector=vpc-id in (42,43,...,51)
  → Watch cache scans ALL objects → O(300K) per LIST
```

### With annotation

```
WATCH: Agent watches with labelSelector=vpc-id in (42,43,...,51)
  → Watcher registered in valueWatchers["42"], ["43"], ..., ["51"]
  → Event for vpc-id=42 looks up only valueWatchers["42"] → O(1)

LIST: Agent LISTs with labelSelector=vpc-id in (42,43,...,51)
  → Watch cache calls ByIndex("l:vpc-id", "42"), ..., ByIndex("l:vpc-id", "51")
  → Returns only matching objects → O(10) lookups instead of O(300K) scan
```

### The "l:" prefix convention

Index names use `"l:" + labelKey` (e.g., `"l:vpc-id"`) to distinguish label-based indexes from field-based indexes (`"f:" + fieldName`). This matches the existing `LabelIndex()` and `FieldIndex()` conventions in `selection_predicate.go`.

## Test Results

Tested at 60K agents, 30 CRD types, 1.8M watches, ~120 patches/sec:

| Metric | Stock K8s (12 servers) | WATCH index only (6 servers) | WATCH + LIST index (6 servers) |
|--------|----------------------|------------------------------|-------------------------------|
| p50 latency | 15.6 ms | — | **5.3 ms** |
| p95 latency | 89 ms | — | **11.9 ms** |
| p99 latency | 269 ms | — | **12.7 ms** |
| Total CPU | ~60 cores | ~18 cores (OOMed) | **~20 cores** |
| API servers | 12 | 6 (crashed) | **6 (stable)** |
| Restarts | 0 | 2-3 each | **0** |

## Files Changed

### Core API server (`staging/src/k8s.io/apiserver/`)

#### `pkg/storage/cacher/cacher.go` — WATCH dispatch indexing

**`indexedWatchers.addWatcher` / `deleteWatcher`** (lines 148-180):
Changed from single `value string` to `values []string`. When `triggerSupported && len(values) > 0`, the watcher is registered in multiple `valueWatchers` buckets — one per value in the `in(...)` set.

```go
// Before: one bucket
i.valueWatchers[value].addWatcher(w, number)

// After: multiple buckets for in(42,43,...,51)
for _, value := range values {
    i.valueWatchers[value].addWatcher(w, number)
}
```

**`Watch()` trigger extraction** (lines 561-587):
Added a label-based trigger path. If the cacher's `indexedTrigger` has an `"l:"`-prefixed index name, it extracts multiple values from the label selector using `RequiresExactMatchOrIn()` instead of the field selector's `RequiresExactMatch()`.

```go
// Existing field-based path (unchanged)
if value, ok := pred.Field.RequiresExactMatch(field); ok { ... }

// New label-based path
if !triggerSupported && strings.HasPrefix(c.indexedTrigger.indexName, "l:") {
    labelKey := strings.TrimPrefix(c.indexedTrigger.indexName, "l:")
    if values, ok := pred.Label.RequiresExactMatchOrIn(labelKey); ok {
        triggerValues = values
        triggerSupported = true
    }
}
```

**`forgetWatcher()`** (line 1229):
Changed closure to capture `triggerValues []string` instead of `triggerValue string`. Cleanup removes the watcher from all indexed buckets.

#### `pkg/storage/cacher/watch_cache.go` — LIST indexing

**`listLatestRV()`** (lines 642-682):
Changed from "return first ByIndex hit" to "group matchValues by index name, merge all ByIndex results for the same index." This handles `in(...)` selectors that produce multiple MatchValues for the same label index.

```go
// Before: returns on first match (misses other values)
for _, matchValue := range matchValues {
    if result, err := w.store.ByIndex(matchValue.IndexName, matchValue.Value); err == nil {
        return ...
    }
}

// After: merges all values for same index
indexGroups := make(map[string][]string)
for _, mv := range matchValues {
    indexGroups[mv.IndexName] = append(indexGroups[mv.IndexName], mv.Value)
}
for indexName, values := range indexGroups {
    var merged []interface{}
    for _, value := range values {
        if result, err := w.store.ByIndex(indexName, value); err == nil {
            merged = append(merged, result...)
        }
    }
    // Return merged results...
}
```

This is safe because `listLatestRV` is documented as "prefiltering" — returning extra items is OK (they get filtered later), but missing items is not.

#### `pkg/storage/selection_predicate.go` — MatcherIndex multi-value support

**`MatcherIndex()`** (lines 174-180):
Changed label matching from `RequiresExactMatch` (returns single value, fails for `in()` with >1 value) to `RequiresExactMatchOrIn` (returns all values). Emits one `MatchValue` per value in the `in(...)` set.

```go
// Before: only worked for = or in(single_value)
if value, ok := s.Label.RequiresExactMatch(label); ok {
    result = append(result, MatchValue{...})
}

// After: works for in(42,43,...,51)
if values, ok := s.Label.RequiresExactMatchOrIn(label); ok {
    for _, value := range values {
        result = append(result, MatchValue{...})
    }
}
```

### Label selector (`staging/src/k8s.io/apimachinery/pkg/labels/`)

#### `selector.go` — RequiresExactMatchOrIn

Added `RequiresExactMatchOrIn(label string) ([]string, bool)` to the `Selector` interface and all three implementations (`internalSelector`, `nothingSelector`, `ValidatedSetSelector`). Returns all values from `Equals`, `DoubleEquals`, or `In` operators. Returns `(nil, false)` for `NotIn`, `Exists`, `DoesNotExist`, or if the label key is not constrained.

### CRD storage (`staging/src/k8s.io/apiextensions-apiserver/`)

#### `pkg/registry/customresource/etcd.go` — TriggerFunc + Indexers

**`NewStorage()`**: Added `watchIndexLabel string` parameter. When non-empty, sets both:
- `options.TriggerFunc` — `IndexerFunc` that extracts the label value from runtime.Object (for WATCH dispatch)
- `options.Indexers` — `cache.IndexFunc` that extracts the label value from cached objects (for LIST filtering)

Both use the `"l:" + labelKey` index name convention.

**`labelIndexerFunc()`**: Returns the label value as a single string (for WATCH `TriggerFunc`).

**`labelCacheIndexFunc()`**: Returns the label value as `[]string` (for LIST `cache.Indexers`).

#### `pkg/registry/customresource/strategy.go` — IndexLabels in SelectionPredicate

Added `watchIndexLabel string` field to `customResourceStrategy`. When set, `MatchCustomResourceDefinitionStorage()` includes it in `pred.IndexLabels`, which causes `MatcherIndex()` to generate indexed MatchValues for LIST operations.

#### `pkg/apiserver/customresource_handler.go` — Annotation extraction

Reads `crd.Annotations["crusoe.ai/watch-index-label"]` and passes it to both `NewStrategy()` and `NewStorage()`.

## Annotation-Gated Behavior

All new code paths are gated. When `watchIndexLabel == ""` (no annotation):

| Component | Behavior |
|-----------|----------|
| `etcd.go NewStorage()` | `TriggerFunc` not set, `Indexers` not set → stock behavior |
| `strategy.go MatchCustomResourceDefinitionStorage()` | `IndexLabels` empty → `MatcherIndex()` returns nothing for labels |
| `cacher.go Watch()` | `indexedTrigger` is nil → `triggerSupported=false` → watcher goes to `allWatchers` |
| `watch_cache.go listLatestRV()` | `matchValues` empty → falls through to `store.List()` or `ListPrefix()` |

**No code path is changed for CRDs without the annotation.** The forked API server is a drop-in replacement for stock K8s v1.36.0 for all non-annotated CRDs.

## Edge Cases

1. **Object with missing label**: `labelIndexerFunc` returns `""`, `labelCacheIndexFunc` returns `[""]`. The ByIndex lookup for `"42"` won't match it. Correct — unlabeled objects shouldn't match `vpc-id in (42,...)`.

2. **Label value changes on UPDATE**: `triggerValuesThreadUnsafe()` returns both old and new values. Watchers in both buckets may receive the event — this is rare (VPC labels don't change) and harmless (client-go informers deduplicate by resourceVersion).

3. **Watcher with no label selector (unfiltered)**: `RequiresExactMatchOrIn` returns `(nil, false)` → `triggerSupported=false` → watcher goes into `allWatchers` → receives all events. Correct.

4. **Watcher with `!=` or `NotIn`**: Not handled by `RequiresExactMatchOrIn` → falls back to `allWatchers` → O(n) but correct. These operators are rare in our use case.

5. **Single-value `in(42)`**: Works — generates 1 triggerValue and 1 MatchValue. Same as exact match path.

6. **Large `in(...)` set (10 values)**: 10 ByIndex calls for LIST, watcher in 10 valueWatchers buckets. Each is O(1). Total O(10) vs O(300K) scan.

## Building

```bash
# Requires Go 1.26.x
gvm use go1.26.1   # or install: gvm install go1.26.1 -B

# Build for Linux
cd ~/Crusoe/k8s-v1.36
GOOS=linux GOARCH=amd64 go build -o kube-apiserver-linux-amd64 ./cmd/kube-apiserver

# Build Docker image
docker build -t kube-apiserver:v1.36.0-label-index-v2 -f - . <<'EOF'
FROM registry.k8s.io/kube-apiserver:v1.36.0
COPY kube-apiserver-linux-amd64 /usr/local/bin/kube-apiserver
EOF
```

## Deploying

1. Push image to registry
2. Update `values.yaml` with the new image
3. Add `crusoe.ai/watch-index-label: "vpc-id"` annotation to CRD YAMLs
4. Deploy: `bash deploy-cisv2.sh --namespace sdn-scale-test --vip <VIP> --release sdn`

The annotation is read at CRD registration time. If you add/remove the annotation on an existing CRD, the API server needs to re-register the CRD handler (restart the API server or trigger CRD re-registration).

## Rebasing on future K8s versions

The changes touch these specific code locations:

| File | Location | Risk |
|------|----------|------|
| `labels/selector.go` | New methods added (additive) | Low — no conflicts unless Selector interface changes |
| `cacher/cacher.go` | `addWatcher`/`deleteWatcher` signatures changed | Medium — if upstream changes these functions |
| `watch_cache.go` | `listLatestRV` rewritten | Medium — if upstream changes LIST path |
| `selection_predicate.go` | `MatcherIndex` label loop | Low — small change |
| `customresource/etcd.go` | `NewStorage` parameter added | Low — additive |
| `customresource/strategy.go` | Field + parameter added | Low — additive |
| `customresource_handler.go` | Annotation read + pass-through | Low — additive |

Most changes are additive. The highest-risk files are `cacher.go` and `watch_cache.go` where upstream may modify the dispatch or list paths.
