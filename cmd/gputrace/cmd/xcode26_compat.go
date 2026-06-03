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
//
// Xcode 26.5 note: the popover's defining signal is the "Start new session"
// AXRadioButton plus the Performance State AXPopUpButton; the inner confirm is
// an AXButton with the exact title "Profile" (NO ellipsis). We detect the
// popover via the radio/popup so we don't mistake the still-present OUTER
// "Profile…" button for a popover that never opened.
func handleProfilePopover(windowAX uintptr) bool {
	// Wait for the popover to materialize. We poll because Xcode's SwiftUI
	// popover animation can take several hundred ms. The reliable signals
	// (verified on Xcode 26.5 / M3) are the "Start new session" radio button
	// and the Performance State popup (an AXPopUpButton whose AXValue is one
	// of Minimum/Medium/Maximum). The inner confirm button alone is NOT a
	// reliable signal because the OUTER "Profile…" button is also titled
	// "Profile" via AXDescription on some builds.
	popoverUp := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if findPopUpByValueSet(windowAX, []string{"Minimum", "Medium", "Maximum"}, 1500) != 0 ||
			findRadioButtonByTitle(windowAX, "Start new session", 1500) != 0 {
			popoverUp = true
			break
		}
	}
	if !popoverUp {
		verboseLog("handleProfilePopover: no Start new session popover detected (outer Profile click started the profile directly, or older Xcode)")
		return false
	}
	verboseLog("handleProfilePopover: Start new session popover detected, applying prefs")

	// Best-effort: tweak Performance State if the user set METALOPTIM_GPU_STATE.
	// We don't fail the whole operation if this doesn't work — defaults are fine.
	if err := applyGPUStatePref(windowAX); err != nil {
		verboseLog("handleProfilePopover: GPU state pref skipped: %v", err)
	}

	// Find the inner confirm button. It's an AXButton with the EXACT title
	// "Profile" (no ellipsis) that is enabled and lives inside the popover.
	// Re-find after dropdown manipulation in case AX references moved.
	innerBtn := findEnabledExactButton(windowAX, "Profile", 1500)
	if innerBtn == 0 {
		verboseLog("handleProfilePopover: inner Profile confirm button not found in popover")
		return false
	}
	if p := findPopUpByValueSet(windowAX, []string{"Minimum", "Medium", "Maximum"}, 2000); p != 0 {
		verboseLog("handleProfilePopover: state popup reads %q just before confirming", axString(p, "AXValue"))
	}
	// Background-only press (no foreground fallback) so we never pull Xcode to
	// the front. AXPress on this confirm button starts the profile-with-
	// counters replay at the state we just selected.
	if err := axPressBackground(innerBtn); err != nil {
		verboseLog("handleProfilePopover: inner Profile click failed: %v", err)
		return false
	}
	verboseLog("handleProfilePopover: inner Profile confirmed (background press)")
	return true
}

// applyGPUStatePref reads METALOPTIM_GPU_STATE (Minimum|Medium|Maximum) and,
// if set, adjusts the Performance State popup in the visible popover.
// Returns nil when the env var is unset or the requested value already
// matches the current selection. Returns a non-nil error if the env var is
// invalid OR if any AX step fails; callers treat this best-effort.
//
// Xcode 26.5 note: the Performance State control is an AXPopUpButton whose
// AXTitle is EMPTY and whose CURRENT SELECTION lives in AXValue (e.g.
// AXValue="Medium"). We therefore match by AXValue (not AXTitle) and open it
// with the AXShowMenu action (the popup exposes both AXShowMenu and AXPress;
// AXShowMenu is the reliable one for SwiftUI popups). The chosen menu item is
// an AXMenuItem with the exact title. Verified live on Xcode 26.5 / M3: this
// reliably flips the popup to the requested state and the resulting replay's
// GPU Time scales accordingly (Minimum≈9.7ms, Medium≈3.6ms, Maximum≈2.5ms on
// the nbody seed).
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

	// Find the Performance State popup by its AXValue (current selection).
	// Other popups in the popover (e.g. GPU Execution Mode = "Overlapping",
	// or "No Saved Profiler Data") will not match this value set.
	popup := findPopUpByValueSet(windowAX, []string{"Minimum", "Medium", "Maximum"}, 2000)
	if popup == 0 {
		// Fall back to the legacy AXTitle-based match in case a future/older
		// Xcode build exposes the selection via AXTitle instead.
		popup = findPopUpByTitleSet(windowAX, []string{"Minimum", "Medium", "Maximum"}, 2000)
	}
	if popup == 0 {
		return fmt.Errorf("Performance State popup not found in popover")
	}
	cur := axString(popup, "AXValue")
	if cur == "" {
		cur = axString(popup, "AXTitle")
	}
	if cur == state {
		verboseLog("applyGPUStatePref: already %s", state)
		return nil
	}

	// Open the popup menu. NOTE (Xcode 26.5): AXShowMenu on this SwiftUI popup
	// returns -25204 (ActionUnsupported) but ACTUALLY OPENS the menu, so we
	// ignore the error and search for the menu item. The menu items appear in
	// the WINDOW subtree (not under the popup) — verified live. Search there
	// first, then the popup subtree, then app-level menus.
	_ = axAction(popup, "AXShowMenu") // error intentionally ignored (opens anyway)
	time.Sleep(400 * time.Millisecond)
	item := findMenuItemInTree(windowAX, state, 5000)
	if item == 0 {
		item = findMenuItemInTree(popup, state, 1500)
	}
	if item == 0 {
		item = findMenuItemByTitle(state, 1500)
	}
	if item == 0 {
		// Menu may not have opened; try a plain background press to open it,
		// then re-search.
		sendEscape()
		time.Sleep(150 * time.Millisecond)
		if err := axPressBackground(popup); err == nil {
			time.Sleep(400 * time.Millisecond)
			item = findMenuItemInTree(windowAX, state, 5000)
		}
	}
	if item == 0 {
		sendEscape() // close the dangling menu so we don't leave Xcode stuck
		return fmt.Errorf("menu item %q not found after opening Performance State menu", state)
	}
	// Background-only press (no foreground fallback): the AppleScript/CGEvent
	// fallback would foreground Xcode AND fails to commit the SwiftUI menu
	// selection. A plain AXPress on this menu item both stays background and
	// commits (verified live: state flips and the replay's GPU Time changes).
	if err := axPressBackground(item); err != nil {
		return fmt.Errorf("failed to select %q menu item: %w", state, err)
	}
	// Brief wait for menu close + popup repaint, then verify.
	time.Sleep(300 * time.Millisecond)
	if p2 := findPopUpByValueSet(windowAX, []string{"Minimum", "Medium", "Maximum"}, 2000); p2 != 0 {
		now := axString(p2, "AXValue")
		if now != state {
			verboseLog("applyGPUStatePref: WARNING Performance State is %q after selecting %q", now, state)
		} else {
			verboseLog("applyGPUStatePref: set Performance State %s -> %s", cur, state)
		}
	}
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

// findPopUpByValueSet BFS-searches `root`'s AX subtree for an AXPopUpButton
// whose AXVALUE is in `values`. Xcode 26.5 leaves the Performance State popup's
// AXTitle empty and exposes the current selection via AXValue, so this is the
// matcher used to locate that popup.
func findPopUpByValueSet(root uintptr, values []string, maxVisit int) uintptr {
	valueSet := make(map[string]struct{}, len(values))
	for _, v := range values {
		valueSet[v] = struct{}{}
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
			if _, ok := valueSet[axString(el, "AXValue")]; ok {
				return el
			}
		}

		children := axChildren(el)
		queue = append(queue, children...)
	}
	return 0
}

// ensureGPUTraceJumpBarView switches the GPU-trace editor's Jump Bar to the
// named view (e.g. "Performance" or "Summary") via the AXShowMenu action on
// the editor's view-selector AXPopUpButton. This is the deterministic lever
// for surfacing view-specific controls:
//   - "Performance" view exposes the outer "Profile…" (ellipsis) button that
//     opens the Start-new-session popover (with the Performance State dropdown).
//   - "Summary" view exposes the Export button used by the export step.
//
// `xp navigator performance` (selecting the left Debug-navigator row) does NOT
// reliably switch the editor content on Xcode 26.5 — verified flaky across
// repeated trials — so callers that need a specific view should call this.
// Returns true if the Jump Bar already showed, or was switched to, `view`.
// Best-effort and background-safe (AX actions only; never foregrounds Xcode).
func ensureGPUTraceJumpBarView(windowAX uintptr, view string) bool {
	viewNames := []string{"Summary", "Performance", "Counters", "Overview", "Timeline", "Shaders"}
	popup := findPopUpByValueSet(windowAX, viewNames, 4000)
	if popup == 0 {
		verboseLog("ensureGPUTraceJumpBarView: no Jump Bar view popup found")
		return false
	}
	if axString(popup, "AXValue") == view {
		verboseLog("ensureGPUTraceJumpBarView: Jump Bar already showing %q", view)
		return true
	}

	// Open the Jump Bar menu. NOTE (Xcode 26.5): AXShowMenu on this SwiftUI
	// popup returns -25204 (ActionUnsupported) but ACTUALLY OPENS the menu —
	// same class of quirk as the Replay button's -25205. So we IGNORE the
	// error and look for the menu items, which materialize in the WINDOW
	// subtree (not under the popup). Only if no menu appears do we fall back.
	openMenuAndFindItem := func(want string) uintptr {
		_ = axAction(popup, "AXShowMenu") // error intentionally ignored (see above)
		time.Sleep(400 * time.Millisecond)
		// Menu items appear under the window; also check popup subtree + app.
		if it := findMenuItemInTree(windowAX, want, 5000); it != 0 {
			return it
		}
		if it := findMenuItemInTree(popup, want, 1500); it != 0 {
			return it
		}
		return findMenuItemByTitle(want, 1500)
	}
	item := openMenuAndFindItem(view)
	if item == 0 {
		// Menu may not have opened via AXShowMenu; try a plain background press.
		sendEscape()
		time.Sleep(150 * time.Millisecond)
		if err := axPressBackground(popup); err == nil {
			time.Sleep(400 * time.Millisecond)
			if it := findMenuItemInTree(windowAX, view, 5000); it != 0 {
				item = it
			}
		}
	}
	if item == 0 {
		sendEscape()
		verboseLog("ensureGPUTraceJumpBarView: menu item %q not found", view)
		return false
	}
	// Background-only press so we never foreground Xcode while switching views.
	if err := axPressBackground(item); err != nil {
		verboseLog("ensureGPUTraceJumpBarView: select %q failed: %v", view, err)
		return false
	}
	// Wait for the editor to repaint into the new view.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if p2 := findPopUpByValueSet(windowAX, viewNames, 4000); p2 != 0 && axString(p2, "AXValue") == view {
			verboseLog("ensureGPUTraceJumpBarView: switched Jump Bar to %q", view)
			return true
		}
	}
	verboseLog("ensureGPUTraceJumpBarView: Jump Bar did not confirm switch to %q", view)
	return false
}

// findRadioButtonByTitle BFS-searches for an AXRadioButton with the given
// AXTitle. Used to detect the "Start new session" radio that marks the
// profile popover.
func findRadioButtonByTitle(root uintptr, title string, maxVisit int) uintptr {
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

		if axString(el, "AXRole") == "AXRadioButton" && axString(el, "AXTitle") == title {
			return el
		}
		queue = append(queue, axChildren(el)...)
	}
	return 0
}

// axPressBackground presses an element via AXPress ONLY, never foregrounding
// Xcode. On Xcode 26.5 the popover's menu items and inner "Profile" confirm
// button respond to a plain AXPress (verified live: returns 0). Some SwiftUI
// controls return -25205/-25204 from AXPress but still fire the action; we
// treat those as success too rather than escalating to the AppleScript/CGEvent
// fallback in axPressWithFallbackWindow, which calls ActivateXcode() and would
// pull Xcode to the foreground (and, empirically, fails to commit the SwiftUI
// popover selection). Background-only by construction.
func axPressBackground(el uintptr) error {
	key := mkString("AXPress")
	defer cfRelease(key)
	err := axPerformAction(el, key)
	if err == kAXErrorSuccess || err == -25205 || err == -25204 {
		return nil
	}
	return fmt.Errorf("AXPress AX error %d", err)
}

// findEllipsisProfileButton BFS-searches for the OUTER Profile button that
// opens the Start-new-session popover. On Xcode 26.5 the Performance view
// exposes it as an AXButton with AXTitle "Profile…" (U+2026) or "Profile..."
// (ASCII). We match ONLY the ellipsis variants so we never pick the Summary
// inspector's plain "Profile" (which re-profiles directly with no popover).
func findEllipsisProfileButton(root uintptr, maxVisit int) uintptr {
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

		if axString(el, "AXRole") == "AXButton" {
			t := axString(el, "AXTitle")
			if t == "" {
				t = axString(el, "AXDescription")
			}
			if t == "Profile…" || t == "Profile..." {
				return el
			}
		}
		queue = append(queue, axChildren(el)...)
	}
	return 0
}

// findEnabledExactButton BFS-searches for an enabled AXButton whose AXTitle is
// EXACTLY `name` (not the ellipsis variants). Used to find the popover's inner
// "Profile" confirm button without matching the outer "Profile…" button.
func findEnabledExactButton(root uintptr, name string, maxVisit int) uintptr {
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

		if axString(el, "AXRole") == "AXButton" && axString(el, "AXTitle") == name && IsElementEnabled(el) {
			return el
		}
		queue = append(queue, axChildren(el)...)
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
