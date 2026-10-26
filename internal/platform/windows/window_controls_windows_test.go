package windows

import (
	"testing"

	"github.com/vivaui/viva/internal/platform"
)

func TestClassifyPointFindsEdgesCornersCaptionAndClient(t *testing.T) {
	client := func() platform.PressKind { return platform.PressClient }
	move := func() platform.PressKind { return platform.PressMove }
	control := func() platform.PressKind { return platform.PressControl }
	onDrag := func() platform.PressKind { return platform.PressMoveOnDrag }
	for _, test := range []struct {
		name   string
		x, y   int32
		scale  float32
		zoomed bool
		press  func() platform.PressKind
		want   uintptr
	}{
		{"top left corner", 2, 2, 1, false, client, htTopLeft},
		{"top right corner", 797, 2, 1, false, client, htTopRight},
		{"bottom left corner", 2, 597, 1, false, client, htBottomLeft},
		{"bottom right corner", 797, 597, 1, false, client, htBottomRight},
		{"left", 2, 300, 1, false, client, htLeft},
		{"right", 797, 300, 1, false, client, htRight},
		{"bottom", 400, 597, 1, false, client, htBottom},
		{"top", 400, 2, 1, false, client, htTop},
		{"left edge near the top is a corner", 2, 10, 1, false, client, htTopLeft},
		{"a move region is the caption", 400, 20, 1, false, move, htCaption},
		{"a control is the client's", 400, 20, 1, false, control, htClient},
		{"a backdrop over a move region is the client's, which moves the window on a drag", 400, 20, 1, false, onDrag, htClient},
		{"the top edge yields to a control", 400, 2, 1, false, control, htClient},
		{"the top edge does not yield to a backdrop", 400, 2, 1, false, onDrag, htTop},
		{"the sides do not yield to a control", 797, 300, 1, false, control, htRight},
		{"the client area", 400, 300, 1, false, client, htClient},
		{"a scaled border", 8, 300, 1.5, false, client, htLeft},
		{"a maximized window has no edges", 2, 300, 1, true, client, htClient},
		{"a maximized window still moves from a move region", 2, 20, 1, true, move, htCaption},
	} {
		if got := classifyPoint(test.x, test.y, 800, 600, test.scale, test.zoomed, test.press); got != test.want {
			t.Errorf("%s: got %d, want %d", test.name, got, test.want)
		}
	}
}

func TestACaptionPressBecomesAMoveOnlyWhenTheButtonStaysDownAndThePointerDrags(t *testing.T) {
	var press captionPress
	if press.dragged(100, 20, true) {
		t.Fatal("a move with no press started a window move")
	}
	press.begin(100, 20)
	if press.dragged(102, 21, true) {
		t.Fatal("a move within the slop started a window move")
	}
	if !press.dragged(110, 24, true) {
		t.Fatal("a drag past the slop did not start a window move")
	}
	if press.dragged(150, 24, true) {
		t.Fatal("the window move was asked for twice")
	}
	press.begin(100, 20)
	if press.dragged(150, 24, false) || press.dragged(150, 24, true) {
		t.Fatal("a press whose button came up still started a window move")
	}
	press.begin(100, 20)
	press.end()
	if press.dragged(150, 24, true) {
		t.Fatal("a released press still started a window move")
	}
}

func TestTheMaximizeButtonIsOnlyWhereTheApplicationDrawsAControl(t *testing.T) {
	control := func(x, y float32) platform.PressKind { return platform.PressControl }
	state := &windowState{config: platform.Config{Actions: &platform.WindowActions{}, PressAt: control, TitlebarHeight: 32}}
	const width = 800
	if !state.overMaximizeButton(width-platform.WindowControlWidth*3/2, 10, width) {
		t.Error("the middle of the maximize button is not over it")
	}
	if state.overMaximizeButton(width-platform.WindowControlWidth/2, 10, width) {
		t.Error("the close button is not the maximize button")
	}
	if state.overMaximizeButton(width-platform.WindowControlWidth*3/2, 40, width) {
		t.Error("below the titlebar strip is not the button")
	}
	state.config.PressAt = func(x, y float32) platform.PressKind { return platform.PressClient }
	if state.overMaximizeButton(width-platform.WindowControlWidth*3/2, 10, width) {
		t.Error("a dialog over the button leaves nothing for the snap layouts to attach to")
	}
	state.config.PressAt = control
	state.config.Actions = nil
	if state.overMaximizeButton(width-platform.WindowControlWidth*3/2, 10, width) {
		t.Error("a window that draws no buttons has no maximize button")
	}
}

// The maximize button is drawn as a client control but the window answers the
// system that it is a non-client button, so that Windows 11 shows its snap
// layouts there. The press and the release therefore reach the application as
// client mouse messages at a point the system does not call client area. The
// client's own mouse-leave tracking must stay off while they are forwarded: a
// client TrackMouseEvent registered there reports the pointer gone at once, and
// the leave cancels the press before the button sees the release, which leaves
// the button looking pressed for an instant and doing nothing.
func TestAPressOnTheMaximizeButtonKeepsTheClientLeaveTrackingOff(t *testing.T) {
	const window = ^uintptr(0)
	state := &windowState{
		ready: true, window: window, scale: 1,
		config: platform.Config{
			WindowMode:     platform.WindowCustomTitlebar,
			TitlebarHeight: 32,
			Actions:        &platform.WindowActions{},
			PressAt:        func(x, y float32) platform.PressKind { return platform.PressControl },
		},
	}
	previous := active.Load()
	windowStates.Store(window, state)
	defer func() { windowStates.Delete(window); active.Store(previous) }()

	var forwarded []uintptr
	leaveTrackingOff := true
	previousMessage := clientMessage
	clientMessage = func(window, kind, first, second uintptr) uintptr {
		forwarded = append(forwarded, kind)
		leaveTrackingOff = leaveTrackingOff && state.tracking
		return 0
	}
	defer func() { clientMessage = previousMessage }()

	at := uintptr(uint16(10)) | uintptr(uint16(18))<<16
	windowProc(window, wmNCLButtonDown, htMaxButton, at)
	windowProc(window, wmNCLButtonUp, htMaxButton, at)

	if len(forwarded) != 2 || forwarded[0] != wmLeftDown || forwarded[1] != wmLeftUp {
		t.Fatalf("forwarded %v, want the press and the release", forwarded)
	}
	if !leaveTrackingOff {
		t.Error("the press reached the application with the client's mouse-leave tracking free to arm, whose leave cancels the press before the release")
	}
	if state.tracking {
		t.Error("the press left the client's mouse-leave tracking on")
	}
}
