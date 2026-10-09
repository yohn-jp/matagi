package presentation

import (
	"errors"
	"fmt"
)

// Schema identifies the separately persisted desktop layout format.
const Schema = "matagi.desktop/1"

// MaxLayoutEntries bounds the number of logical views accepted from storage.
const MaxLayoutEntries = 128

// MaxLayoutBytes bounds the desktop layout file before JSON decoding.
const MaxLayoutBytes int64 = 256 * 1024

// Layout contains only logical identities and presentation placement.
type Layout struct {
	Views    []LayoutEntry `json:"views"`
	Selected *EndpointKey  `json:"selected,omitempty"`
}

// LayoutEntry stores one logical endpoint's tab order, target location, and
// optional normal detached-window bounds in DIPs.
type LayoutEntry struct {
	Key      EndpointKey `json:"key"`
	Location Location    `json:"location"`
	Bounds   *DIPBounds  `json:"bounds,omitempty"`
}

// LoadProblem classifies a nonfatal issue that leaves the saved file untouched.
type LoadProblem string

const (
	LoadProblemMalformed  LoadProblem = "malformed"
	LoadProblemTooLarge   LoadProblem = "too-large"
	LoadProblemUnknown    LoadProblem = "unknown-schema"
	LoadProblemUnreadable LoadProblem = "unreadable"
)

// LoadSummary is a bounded report of entries that could not be restored.
type LoadSummary struct {
	Problem     LoadProblem
	Skipped     int
	MoreSkipped bool
}

var errInvalidLayout = errors.New("desktop layout is invalid")

func validateLayout(layout Layout, validKeys map[EndpointKey]struct{}, enforceKeySet bool) error {
	if len(layout.Views) > MaxLayoutEntries {
		return fmt.Errorf("%w: more than %d entries", errInvalidLayout, MaxLayoutEntries)
	}
	seen := make(map[EndpointKey]struct{}, len(layout.Views))
	for index, entry := range layout.Views {
		if !entry.Key.Valid() {
			return fmt.Errorf("%w: entry %d has an invalid endpoint key", errInvalidLayout, index)
		}
		if enforceKeySet {
			if _, ok := validKeys[entry.Key]; !ok {
				return fmt.Errorf("%w: entry %d does not match a registered endpoint", errInvalidLayout, index)
			}
		}
		if _, duplicate := seen[entry.Key]; duplicate {
			return fmt.Errorf("%w: entry %d duplicates an endpoint key", errInvalidLayout, index)
		}
		seen[entry.Key] = struct{}{}
		if !entry.Location.valid() {
			return fmt.Errorf("%w: entry %d has an invalid location", errInvalidLayout, index)
		}
		if entry.Bounds != nil && (!entry.Bounds.valid() || entry.Location == LocationTab) {
			return fmt.Errorf("%w: entry %d has invalid detached-window bounds", errInvalidLayout, index)
		}
	}
	if layout.Selected != nil {
		if !layout.Selected.Valid() {
			return fmt.Errorf("%w: selected endpoint key is invalid", errInvalidLayout)
		}
		if _, ok := seen[*layout.Selected]; !ok {
			return fmt.Errorf("%w: selected endpoint is not present", errInvalidLayout)
		}
	}
	return nil
}

func cloneLayout(layout Layout) Layout {
	copy := Layout{Views: make([]LayoutEntry, len(layout.Views))}
	for index, entry := range layout.Views {
		copy.Views[index] = LayoutEntry{
			Key:      entry.Key,
			Location: entry.Location,
			Bounds:   cloneBounds(entry.Bounds),
		}
	}
	if layout.Selected != nil {
		selected := *layout.Selected
		copy.Selected = &selected
	}
	return copy
}
