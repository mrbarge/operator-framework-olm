package semver

import (
	"fmt"
	"io"

	"github.com/blang/semver/v4"
	"github.com/operator-framework/operator-registry/pkg/image"
)

// data passed into this module externally
type Template struct {
	Data     io.Reader
	Registry image.Registry
}

// IO structs -- BEGIN
type semverTemplateBundleEntry struct {
	Image string `json:"image,omitempty"`
}

type semverTemplateChannelBundles struct {
	Bundles []semverTemplateBundleEntry `json:"bundles,omitempty"`
}

type semverTemplate struct {
	Schema                string                       `json:"schema"`
	GenerateMajorChannels bool                         `json:"generateMajorChannels,omitempty"`
	GenerateMinorChannels bool                         `json:"generateMinorChannels,omitempty"`
	Candidate             semverTemplateChannelBundles `json:"candidate,omitempty"`
	Fast                  semverTemplateChannelBundles `json:"fast,omitempty"`
	Stable                semverTemplateChannelBundles `json:"stable,omitempty"`

	pkg            string `json:"-"` // the derived package name
	defaultChannel string `json:"-"` // detected "most stable" channel head
}

// IO structs -- END

const schema string = "olm.semver"

// channel "archetypes", restricted in this iteration to just these
type channelArchetype string

const (
	candidateChannelArchetype channelArchetype = "candidate"
	fastChannelArchetype      channelArchetype = "fast"
	stableChannelArchetype    channelArchetype = "stable"
)

// mapping channel name --> stability, where higher values indicate greater stability
var channelPriorities = map[channelArchetype]int{candidateChannelArchetype: 0, fastChannelArchetype: 1, stableChannelArchetype: 2}

// sorting capability for a slice according to the assigned channelPriorities
type byChannelPriority []channelArchetype

func (b byChannelPriority) Len() int { return len(b) }
func (b byChannelPriority) Less(i, j int) bool {
	return channelPriorities[b[i]] < channelPriorities[b[j]]
}
func (b byChannelPriority) Swap(i, j int) { b[i], b[j] = b[j], b[i] }

type streamType string

const minorStreamType streamType = "minor"
const majorStreamType streamType = "major"

var streamTypePriorities = map[streamType]int{minorStreamType: 0, majorStreamType: 1}

// map of archetypes --> bundles --> bundle-version from the input file
type bundleVersions map[channelArchetype]map[string]semver.Version // e.g. srcv["stable"]["example-operator.v1.0.0"] = 1.0.0

// the "high-water channel" struct functions as a freely-rising indicator of the "most stable" channel head, so we can use that
// later as the package's defaultChannel attribute
type highwaterChannel struct {
	archetype channelArchetype
	version   semver.Version
	name      string
}

func (h *highwaterChannel) gt(ih *highwaterChannel) bool {
	return (channelPriorities[h.archetype] > channelPriorities[ih.archetype]) || (h.version.GT(ih.version))
}

type entryTuple struct {
	arch    channelArchetype
	kind    streamType
	name    string
	parent  string
	index   int
	version semver.Version
}

func (t entryTuple) String() string {
	return fmt.Sprintf("{ arch: %q, kind: %q, name: %q, parent: %q, index: %d, version: %v }", t.arch, t.kind, t.name, t.parent, t.index, t.version.String())
}
