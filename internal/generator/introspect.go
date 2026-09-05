package generator

// introspect.go — reading a generated project back.
//
// Every generator in this package writes through upsertBlock, between a pair of
// markers, with a checksum stamped into the start marker. That was built for
// re-generation: a block whose body still matches its stamp was not touched by
// hand, so replacing it loses nothing, and one that does not match was edited
// and must not be overwritten without --force.
//
// The same three facts — which blocks exist, what is in them, and whether they
// are still pristine — are exactly what a caller needs to answer "what would
// this change?" before changing anything. This file exposes them, and nothing
// else: no new parsing, no second notion of what a block is. If upsertBlock's
// format ever changes, these change with it, because they call the same
// functions it does.

// The generated file names and marker prefixes, published so a reader outside
// this package can look up a block without hard-coding the strings the writers
// use.
const (
	FeaturesFileName    = featuresFileName
	FeatureMarkerPrefix = featureMarkerPrefix
	FeatureCallsBlock   = callsBlockName
	RegistryFileName    = registryFileName
	RouteMarkerPrefix   = routeMarkerPrefix
)

// BlockInfo is the state of one marker block on disk.
type BlockInfo struct {
	// File is the file the block lives in, and Name the block's name inside it.
	File string `json:"file"`
	Name string `json:"name"`

	// Body is the block's contents without its markers.
	Body string `json:"body"`

	// Stamp is the checksum recorded in the start marker, or "" for a block
	// written before stamping or by hand.
	Stamp string `json:"stamp"`

	// Pristine reports that Body still hashes to Stamp — the block is as
	// generated, so regenerating it discards nothing.
	//
	// An unstamped block is never pristine. That is deliberate: with no
	// recorded hash there is no evidence either way, and the safe reading of
	// "no evidence" is "assume someone edited it".
	Pristine bool `json:"pristine"`
}

// ReadBlock returns the state of one block, and whether it exists at all.
func ReadBlock(fileName, prefix, name string) (BlockInfo, bool) {
	raw, ok := readBlock(fileName, prefix, name)
	if !ok {
		return BlockInfo{}, false
	}

	start, end := markersFor(prefix, name)
	body, stamp := splitBlock(raw, start, end)

	return BlockInfo{
		File:     fileName,
		Name:     name,
		Body:     body,
		Stamp:    stamp,
		Pristine: blockIsPristine(body, stamp),
	}, true
}

// ListBlocks returns the block names present in a file, in the order the file
// declares them.
func ListBlocks(fileName, prefix string) []string { return listBlocks(fileName, prefix) }

// InstalledFeatures returns the features wired into the project in the working
// directory, in file order.
//
// The calls block is filtered out: it is the dispatcher rebuilt from the other
// blocks, not a feature.
func InstalledFeatures() []string {
	names := listBlocks(featuresFileName, featureMarkerPrefix)
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name == callsBlockName {
			continue
		}
		out = append(out, name)
	}
	return out
}

// FleetTransports returns the transports the schema accepts, and the subset the
// generators can actually emit.
//
// Both are returned together because reporting one without the other is how a
// caller ends up believing "grpc" is available: it is a valid configuration
// value, and generation still refuses it.
func FleetTransports() (accepted, implemented []string) {
	return append([]string(nil), fleetTransports...), append([]string(nil), fleetImplementedTransports...)
}

// FleetBackends is FleetTransports for the events transport's backends.
func FleetBackends() (accepted, implemented []string) {
	return append([]string(nil), fleetBackends...), append([]string(nil), fleetImplementedBackends...)
}

// RouteEntry is one route registration found in routes_generated.go.
//
// It is the same value `breeze routes --json` prints, exported so a caller can
// have the routes without parsing that output back.
type RouteEntry = routeEntry

// ParseRoutes returns the routes registered in fileName, by parsing it.
//
// This is the parser `breeze routes` runs: routes are read out of the source
// rather than from a list maintained beside it, so a route added by hand inside
// a marker block is found the same as a generated one.
func ParseRoutes(fileName string) ([]RouteEntry, error) { return parseRoutes(fileName) }

// CurrentModulePath reads the module path from the go.mod in the working
// directory. It is how a generator learns the import prefix for the code it is
// about to emit.
func CurrentModulePath() (string, error) { return currentModulePath() }
