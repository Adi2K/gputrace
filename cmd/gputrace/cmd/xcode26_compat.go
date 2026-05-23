// Xcode 26 compatibility helpers for the GPU profiling UI.
//
// Xcode 26 made two changes that broke gputrace's existing automation:
//
//  1. The "Profile" button on a trace's Performance tab is now rendered with
//     a trailing ellipsis ("Profile..." or "Profile…", U+2026) because clicking
//     it now opens a "Start new session" popover instead of starting the
//     profile directly. matchButtonName + findButtonBFSLoose handle either
//     label form.
//
//  2. After clicking the outer "Profile..." button, a popover opens with:
//     - Performance State popup (Minimum/Medium/Maximum)
//     - GPU Execution Mode popup (Overlapping/...)
//     - An inner "Profile" button (no ellipsis) to confirm.
//     handleProfilePopover finds and clicks the inner button. If
//     METALOPTIM_GPU_STATE is set to one of {Minimum, Medium, Maximum},
//     applyGPUStatePref will (best-effort) adjust the Performance State
//     popup before confirming.
//
// All popover handling is best-effort: if any step fails (e.g. older Xcode
// where no popover appears, or AX tree differs from what we expect), the
// helpers log via verboseLog and return false rather than aborting the run.

package cmd

import (
	"fmt"
	"os"
	"time"
)

// matchButtonName reports whether `title` matches `base` exactly, or with a
// trailing "..." (three ASCII dots) or "…" (U+2026, the horizontal ellipsis
// character that Apple's UI sometimes substitutes).
func matchButtonName(title, base string) bool {
	return title == base ||
		title == base+"..." ||
		title == base+"…"
}

// findButtonBFSLoose is identical to findButtonBFS but accepts ellipsis
// variants of `name` via matchButtonName.
func findButtonBFSLoose(root uintptr, name string, maxVisit int) uintptr {
	queue := []uintptr{root}
	visited := 0
	seen := make(map[uintptr]bool)

	for len(queue) > 0 && visited < maxVisit {
		el := queue[0]
		queue = queue[1:]

		if seen[el] {
			continue
		}
		seen[el] = true
		visited++

		role := axString(el, "AXRole")
		if role == "AXButton" {
			title := axString(el, "AXTitle")
			if title == "" {
				title = axString(el, "AXDescription")
			}
			if matchButtonName(title, name) {
				return el
			}
		}

		children := axChildren(el)
		queue = append(queue, children...)
	}
	return 0
}

// handleProfilePopover detects and confirms the "Start new session" popover
// that Xcode 26 opens when clicking the outer Profile... button. Returns true
// only when an inner Profile button was found AND successfully clicked.
//
// On older Xcode versions (or any path where the outer click was the full
// action), no popover appears, this returns false, and the caller treats the
// outer click as complete.
func handleProfilePopover(windowAX uintptr) bool {
	// Wait for the popover to materialize. The defining signal is an inner
	// Profile button (exact title, NO ellipsis) appearing in the window AX
	// tree. We poll because Xcode's popover animation can take several hundred
	// ms.
	var innerBtn uintptr
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		// Strict search (findButtonBFS, not ...Loose) — we WANT to skip the
		// outer "Profile..." we just clicked.
		innerBtn = findButtonBFS(windowAX, "Profile", 500)
		if innerBtn != 0 && IsElementEnabled(innerBtn) {
			break
		}
		innerBtn = 0
	}
	if innerBtn == 0 {
		verboseLog("handleProfilePopover: no Start new session popover detected (probably older Xcode)")
		return false
	}
	verboseLog("handleProfilePopover: inner Profile button found, applying prefs")

	// Best-effort: tweak Performance State if the user set METALOPTIM_GPU_STATE.
	// We don't fail the whole operation if this doesn't work — defaults are fine.
	if err := applyGPUStatePref(windowAX); err != nil {
		verboseLog("handleProfilePopover: GPU state pref skipped: %v", err)
	}

	// Re-find the inner button in case the dropdown manipulation moved AX
	// references around.
	innerBtn = findButtonBFS(windowAX, "Profile", 500)
	if innerBtn == 0 {
		verboseLog("handleProfilePopover: inner Profile button vanished after dropdown nav")
		return false
	}
	if err := axPressWithFallbackWindow(innerBtn, windowAX); err != nil {
		verboseLog("handleProfilePopover: inner Profile click failed: %v", err)
		return false
	}
	return true
}

// applyGPUStatePref reads METALOPTIM_GPU_STATE (Minimum|Medium|Maximum) and,
// if set, adjusts the Performance State popup in the visible popover.
// Returns nil when the env var is unset or the requested value already
// matches the current selection. Returns a non-nil error if the env var is
// invalid OR if any AX step fails; callers treat this best-effort.
func applyGPUStatePref(windowAX uintptr) error {
	state := os.Getenv("METALOPTIM_GPU_STATE")
	if state == "" {
		return nil
	}
	switch state {
	case "Minimum", "Medium", "Maximum":
	default:
		return fmt.Errorf("invalid METALOPTIM_GPU_STATE=%q (want Minimum|Medium|Maximum)", state)
	}

	// The Performance State popup's AXTitle equals its current selection.
	// Find a popup whose title is one of the three options. Other popups in
	// the popover (e.g. GPU Execution Mode = "Overlapping") will not match.
	popup := findPopUpByTitleSet(windowAX, []string{"Minimum", "Medium", "Maximum"}, 1000)
	if popup == 0 {
		return fmt.Errorf("Performance State popup not found in popover")
	}
	cur := axString(popup, "AXTitle")
	if cur == state {
		verboseLog("applyGPUStatePref: already %s", state)
		return nil
	}

	if err := axPressWithFallbackWindow(popup, windowAX); err != nil {
		return fmt.Errorf("failed to open Performance State menu: %w", err)
	}
	time.Sleep(250 * time.Millisecond) // menu open animation

	item := findMenuItemByTitle(state, 500)
	if item == 0 {
		return fmt.Errorf("menu item %q not found after opening Performance State menu", state)
	}
	if err := axPressWithFallbackWindow(item, windowAX); err != nil {
		return fmt.Errorf("failed to select %q menu item: %w", state, err)
	}
	verboseLog("applyGPUStatePref: set Performance State %s -> %s", cur, state)
	// Brief wait for menu close + popup repaint
	time.Sleep(200 * time.Millisecond)
	return nil
}

// findPopUpByTitleSet BFS-searches `root`'s AX subtree for an AXPopUpButton
// whose AXTitle is in `titles`. Used to find a popup button labeled by its
// currently-selected value.
func findPopUpByTitleSet(root uintptr, titles []string, maxVisit int) uintptr {
	titleSet := make(map[string]struct{}, len(titles))
	for _, t := range titles {
		titleSet[t] = struct{}{}
	}

	queue := []uintptr{root}
	visited := 0
	seen := make(map[uintptr]bool)

	for len(queue) > 0 && visited < maxVisit {
		el := queue[0]
		queue = queue[1:]

		if seen[el] {
			continue
		}
		seen[el] = true
		visited++

		if axString(el, "AXRole") == "AXPopUpButton" {
			if _, ok := titleSet[axString(el, "AXTitle")]; ok {
				return el
			}
		}

		children := axChildren(el)
		queue = append(queue, children...)
	}
	return 0
}

// findMenuItemByTitle scans the Xcode app's AX windows for an open AXMenu
// containing an AXMenuItem with the given title. Used after pressing an
// AXPopUpButton.
func findMenuItemByTitle(name string, maxVisit int) uintptr {
	appAX, _ := FindXcodeApp()
	if appAX == 0 {
		return 0
	}
	defer cfRelease(appAX)

	windows := GetAllWindows(appAX)
	for _, w := range windows {
		if item := findMenuItemInTree(w, name, maxVisit); item != 0 {
			return item
		}
	}
	return 0
}

func findMenuItemInTree(root uintptr, name string, maxVisit int) uintptr {
	queue := []uintptr{root}
	visited := 0
	seen := make(map[uintptr]bool)

	for len(queue) > 0 && visited < maxVisit {
		el := queue[0]
		queue = queue[1:]

		if seen[el] {
			continue
		}
		seen[el] = true
		visited++

		if axString(el, "AXRole") == "AXMenuItem" {
			if axString(el, "AXTitle") == name {
				return el
			}
		}

		children := axChildren(el)
		queue = append(queue, children...)
	}
	return 0
}
