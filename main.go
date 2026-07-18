package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	idDisplay    = 1
	idRegistry   = 2
	idSync       = 3
	idShm        = 4
	idScreencopy = 5
	idDataCtl    = 6
	idSeat       = 7
	idCompositor = 8
	idLayerShell = 9
	idXdgOutput  = 10
	idDynamic    = 11
)

const (
	roleOutput    = iota // wl_output
	roleFrame            // zwlr_screencopy_frame_v1
	rolePool             // wl_shm_pool
	roleBuffer           // wl_buffer
	roleSurface          // wl_surface
	roleLayer            // zwlr_layer_surface_v1
	roleCallback         // wl_callback (frame)
	roleXdgOutput        // zxdg_output_v1
	roleCount
)

const (
	// zwlr_screencopy_frame_v1
	evBuffer     = 0
	evFlags      = 1
	evReady      = 2
	evFailed     = 3
	evBufferDone = 6
	// zwlr_data_control_source_v1
	evSend      = 0
	evCancelled = 1
	// zwlr_layer_surface_v1
	evLayerConfigure = 0
	evLayerClosed    = 1
	// wl_keyboard
	evKeymap = 0
	evKey    = 3
	// wl_pointer
	evEnter  = 0
	evMotion = 2
	evButton = 3
	// wl_callback
	evDone = 0
	// zxdg_output_v1
	evLogicalPosition = 0
)

const (
	keyEnter     = 28
	keyKpEnter   = 96
	keyEscape    = 1
	statePressed = 1 // wl_keyboard key + wl_pointer button
)

const (
	shadeFull = 256
	shadeDim  = 120
	fadeStep  = 16
	// Selection outline thickness in pixels
	borderW = 3
)

const (
	// wl_shm ARGB8888
	shmARGB = 0
	// wl_shm XRGB8888
	shmXRGB = 1
)

// ----------------------------------------------------------------------------
// Wire
// ----------------------------------------------------------------------------

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)

	return b
}

func i32(v int32) []byte {
	return u32(uint32(v))
}

// Encodes a Wayland string
// (length including NULL then bytes padded to 4)
func str(s string) []byte {
	l := len(s) + 1
	out := make([]byte, 4+((l+3)&^3))

	binary.LittleEndian.PutUint32(out, uint32(l))
	copy(out[4:], s)

	return out
}

// Encodes registry.bind
func newIDArg(iface string, ver, id uint32) []byte {
	return append(append(str(iface), u32(ver)...), u32(id)...)
}

func readU32(b []byte, off int) uint32 {
	return binary.LittleEndian.Uint32(b[off:])
}

func readStr(b []byte, off int) (string, int) {
	l := int(binary.LittleEndian.Uint32(b[off:]))
	// There's also a trailing NUL that we don't need
	s := string(b[off+4 : off+4+l-1])

	return s, off + 4 + ((l + 3) &^ 3)
}

// ----------------------------------------------------------------------------
// Connection
// ----------------------------------------------------------------------------

type conn struct {
	uc   *net.UnixConn
	rbuf []byte
	// These are recv via SCM_RIGHTS and consumed in order
	fds []int

	nextID uint32
	objs   map[uint32]objRef

	pointerID, keyboardID, dataDevID, sourceID uint32
}

type objRef struct {
	idx  int
	role int
}

func (c *conn) allocID() uint32 {
	id := c.nextID
	c.nextID++
	return id
}

func (c *conn) newObj(o *output, role int) uint32 {
	id := c.allocID()
	o.ids[role] = id
	c.objs[id] = objRef{o.idx, role}
	return id
}

func (c *conn) decode(id uint32) (idx, role int, ok bool) {
	ref, ok := c.objs[id]
	return ref.idx, ref.role, ok
}

func (c *conn) frame(obj, opcode uint32, args [][]byte) []byte {
	body := 0

	for _, a := range args {
		body += len(a)
	}

	size := 8 + body
	msg := make([]byte, 8, size)

	binary.LittleEndian.PutUint32(msg[0:], obj)
	binary.LittleEndian.PutUint32(msg[4:], uint32(size)<<16|opcode)

	for _, a := range args {
		msg = append(msg, a...)
	}

	return msg
}

var debug = os.Getenv("SCREENUTIL_DEBUG") != ""

func init() {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

// Timestamp of the last traced wire message.
var tracePrev time.Time

func traceWire(dir string, attrs ...any) {
	if !debug {
		return
	}
	now := time.Now()

	var dus int64
	if !tracePrev.IsZero() {
		dus = now.Sub(tracePrev).Microseconds()
	}
	tracePrev = now

	slog.Debug(dir, append([]any{"+us", dus}, attrs...)...)
}

func span(name string) func() {
	if !debug {
		return func() {}
	}

	start := time.Now()
	return func() {
		slog.Debug("span", "name", name, "us", time.Since(start).Microseconds())
	}
}

func (c *conn) request(obj, opcode uint32, args ...[]byte) {
	msg := c.frame(obj, opcode, args)
	traceWire("request", "obj", obj, "op", opcode, "size", len(msg), "body", msg[8:])

	if _, err := c.uc.Write(msg); err != nil {
		slog.Error("Write request", "err", err)
		os.Exit(1)
	}
}

func (c *conn) requestFD(obj, opcode uint32, fd int, args ...[]byte) {
	oob := syscall.UnixRights(fd)

	msg := c.frame(obj, opcode, args)
	traceWire("request+fd", "obj", obj, "op", opcode, "size", len(msg), "fd", fd, "body", msg[8:])

	if _, _, err := c.uc.WriteMsgUnix(msg, oob, nil); err != nil {
		slog.Error("Write request with fd", "err", err)
		os.Exit(1)
	}
}

func (c *conn) fill() {
	tmp := make([]byte, 1<<16)
	oob := make([]byte, 256)

	n, oobn, _, _, err := c.uc.ReadMsgUnix(tmp, oob)
	if err != nil {
		slog.Error("Read from socket", "err", err)
		os.Exit(1)
	}

	c.rbuf = append(c.rbuf, tmp[:n]...)
	if oobn > 0 {
		c.recvFDs(oob[:oobn])
	}
}

func (c *conn) recvFDs(oob []byte) {
	scms, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		slog.Error("Parse socket control message", "err", err)
		os.Exit(1)
	}

	for _, scm := range scms {
		fds, err := syscall.ParseUnixRights(&scm)
		if err == nil {
			c.fds = append(c.fds, fds...)
		}
	}
}

func (c *conn) dropHeadFD() {
	if len(c.fds) > 0 {
		syscall.Close(c.fds[0])
		c.fds = c.fds[1:]
	}
}

func (c *conn) read() (uint32, uint32, []byte) {
	for len(c.rbuf) < 8 {
		c.fill()
	}

	obj := binary.LittleEndian.Uint32(c.rbuf[0:])
	word := binary.LittleEndian.Uint32(c.rbuf[4:])
	size, op := int(word>>16), word&0xffff

	for len(c.rbuf) < size {
		c.fill()
	}

	body := append([]byte(nil), c.rbuf[8:size]...)
	c.rbuf = append([]byte(nil), c.rbuf[size:]...)

	traceWire("event", "obj", obj, "op", op, "size", size, "body", body)

	if obj == idDisplay && op == 0 { // wl_display.error
		eObj, code := readU32(body, 0), readU32(body, 4)
		msg, _ := readStr(body, 8)

		slog.Error("Wayland protocol error", "object", eObj, "code", code, "msg", msg)
		os.Exit(1)
	}

	if obj == c.keyboardID && op == evKeymap {
		c.dropHeadFD()
	}

	return obj, op, body
}

// ----------------------------------------------------------------------------
// Outputs
// ----------------------------------------------------------------------------

// This is one monitor, and also its position in the global compositor space
// and its captured pixels
type output struct {
	// Registry global name
	name uint32
	idx  int
	x, y int
	// Comes from the screencopy frame
	w, h int
	// Also comes from the screencopy frame
	stride int
	format uint32

	// Shm buffer shared with the compositor (also the surface's attached buffer)
	mem  []byte
	file *os.File
	// Untouched capture
	orig []byte
	// Orig dimmed at the current shade
	base []byte

	ids [roleCount]uint32
}

func (o *output) id(role int) uint32 { return o.ids[role] }

func (o *output) region() sel {
	return sel{o.x, o.y, o.x + o.w, o.y + o.h}
}

func bounds(outs []*output) sel {
	b := outs[0].region()
	for _, o := range outs[1:] {
		b.x0 = min(b.x0, o.x)
		b.y0 = min(b.y0, o.y)
		b.x1 = max(b.x1, o.x+o.w)
		b.y1 = max(b.y1, o.y+o.h)
	}
	return b
}

func intersects(o *output, s sel) bool {
	if s.empty() {
		return false
	}
	return s.x0 < o.x+o.w && s.x1 > o.x && s.y0 < o.y+o.h && s.y1 > o.y
}

// ----------------------------------------------------------------------------
// Capture
// ----------------------------------------------------------------------------

func (c *conn) captureParams(scVer uint32, frameID uint32) (format, w, h, stride uint32) {
	for {
		id, op, body := c.read()

		if id == frameID {
			switch op {
			case evBuffer:
				if f := readU32(body, 0); f == shmARGB || f == shmXRGB {
					format, w, h, stride = f, readU32(body, 4), readU32(body, 8), readU32(body, 12)
				}
			case evBufferDone:
				if stride == 0 {
					slog.Error("Compositor offered no supported shm buffer format")
					os.Exit(1)
				}
				return
			case evFailed:
				slog.Error("Compositor failed to describe the frame")
				os.Exit(1)
			}
		}

		if scVer < 3 && stride != 0 { // these have no buffer_done
			return
		}
	}
}

func (c *conn) waitReady(frameID uint32) (yInvert bool) {
	for {
		id, op, body := c.read()

		if id == frameID {
			switch op {
			case evFlags:
				yInvert = readU32(body, 0)&1 != 0
			case evReady:
				return
			case evFailed:
				slog.Error("Compositor failed to copy the frame")
				os.Exit(1)
			}
		}
	}
}

// Grabs every output into its own shm buffer sequentially.
func (c *conn) captureAll(outs []*output, scVer uint32) {
	for _, o := range outs {
		frameID := c.newObj(o, roleFrame)

		// capture_output(frame, overlay_cursor=0, output)
		c.request(idScreencopy, 0, u32(frameID), u32(0), u32(o.id(roleOutput)))
		format, w, h, stride := c.captureParams(scVer, frameID)

		o.format, o.w, o.h, o.stride = format, int(w), int(h), int(stride)
		o.mem, o.file = c.shmBuffer(o, w, h, stride, format)

		c.request(frameID, 0, u32(o.id(roleBuffer))) // frame.copy
		if c.waitReady(frameID) {
			flipRows(o.mem, o.stride, o.h)
		}

		o.orig = append([]byte(nil), o.mem...)
		o.base = make([]byte, len(o.mem))
	}
}

// ----------------------------------------------------------------------------
// Selection
// ----------------------------------------------------------------------------

// Rectangle in the global compositor coordinate space.
type sel struct {
	x0, y0, x1, y1 int
}

func (s sel) empty() bool {
	return s.x0 >= s.x1 || s.y0 >= s.y1
}

func selFrom(ax, ay, bx, by int, bb sel) sel {
	x0, x1 := order(ax, bx)
	y0, y1 := order(ay, by)

	return sel{
		clamp(x0, bb.x0, bb.x1), clamp(y0, bb.y0, bb.y1),
		clamp(x1, bb.x0, bb.x1), clamp(y1, bb.y0, bb.y1),
	}
}

func order(a, b int) (int, int) {
	if a > b {
		return b, a
	}
	return a, b
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ----------------------------------------------------------------------------
// Compositing
// ----------------------------------------------------------------------------

func dimAll(dst, orig []byte, shade int) {
	defer span("dimAll")()
	for o := 0; o+3 < len(orig); o += 4 {
		dst[o+0] = byte(int(orig[o+0]) * shade >> 8)
		dst[o+1] = byte(int(orig[o+1]) * shade >> 8)
		dst[o+2] = byte(int(orig[o+2]) * shade >> 8)
		dst[o+3] = orig[o+3]
	}
}

func paintOutput(o *output, s sel) {
	defer span("paintOutput")()
	copy(o.mem, o.base)

	gx0, gy0 := max(s.x0, o.x), max(s.y0, o.y)
	gx1, gy1 := min(s.x1, o.x+o.w), min(s.y1, o.y+o.h)
	if s.empty() || gx0 >= gx1 || gy0 >= gy1 {
		// Selection does not touch this output
		return
	}

	for gy := gy0; gy < gy1; gy++ {
		row := (gy - o.y) * o.stride
		lo, hi := row+(gx0-o.x)*4, row+(gx1-o.x)*4
		copy(o.mem[lo:hi], o.orig[lo:hi])
	}
	borderClipped(o, s, gx0, gy0, gx1, gy1)
}

func borderClipped(o *output, s sel, gx0, gy0, gx1, gy1 int) {
	for gy := gy0; gy < gy1; gy++ {
		onH := gy < s.y0+borderW || gy >= s.y1-borderW
		row := (gy - o.y) * o.stride

		for gx := gx0; gx < gx1; gx++ {
			onV := gx < s.x0+borderW || gx >= s.x1-borderW

			if onH || onV {
				borderPixel(o.mem, row+(gx-o.x)*4)
			}
		}
	}
}

func borderPixel(mem []byte, o int) {
	mem[o+0], mem[o+1], mem[o+2], mem[o+3] = 255, 255, 255, 255
}

// ----------------------------------------------------------------------------
// Input
// ----------------------------------------------------------------------------

type drag struct {
	s sel
	// Anchor set on button down, in global coords
	ax, ay int
	// Last pointer position, in global coords
	gx, gy int
	active bool
	// Output the pointer is currently over (set by enter)
	over *output
}

func (c *conn) pointer(d *drag, outs []*output, bb sel, op uint32, body []byte) (done, confirmed bool) {
	switch op {
	case evEnter:
		if i, role, ok := c.decode(readU32(body, 4)); ok && role == roleSurface {
			d.over = outs[i]
		}
		lx, ly := fixedXY(body, 8)
		d.setGlobal(lx, ly)
	case evMotion:
		lx, ly := fixedXY(body, 4)
		d.setGlobal(lx, ly)
		if d.active {
			d.s = selFrom(d.ax, d.ay, d.gx, d.gy, bb)
		}
	case evButton:
		return d.press(readU32(body, 12) == statePressed)
	}

	return false, false
}

func (d *drag) setGlobal(lx, ly int) {
	if d.over == nil {
		return
	}
	d.gx, d.gy = lx+d.over.x, ly+d.over.y
}

func (d *drag) press(down bool) (done, confirmed bool) {
	if down {
		d.ax, d.ay = d.gx, d.gy
		d.s = sel{}
		d.active = true

		return false, false
	}

	d.active = false
	return !d.s.empty(), !d.s.empty()
}

// Reads two adjacent wl_fixed values
func fixedXY(body []byte, off int) (int, int) {
	x := int(int32(readU32(body, off))) >> 8
	y := int(int32(readU32(body, off+4))) >> 8

	return x, y
}

func keyAction(body []byte, hasSelection bool) (done, confirmed bool) {
	if readU32(body, 12) != statePressed {
		return false, false
	}

	switch readU32(body, 8) {
	case keyEscape:
		return true, false
	case keyEnter, keyKpEnter:
		return hasSelection, hasSelection
	}

	return false, false
}

// ----------------------------------------------------------------------------
// Overlay
// ----------------------------------------------------------------------------

// Publishes one output's curernt bufer
func (c *conn) commitOutput(o *output, clock bool) {
	sid := o.id(roleSurface)

	if clock {
		if o.ids[roleCallback] == 0 {
			c.newObj(o, roleCallback)
		}
		c.request(sid, 3, u32(o.id(roleCallback))) // wl_surface.frame
	}
	c.request(sid, 1, u32(o.id(roleBuffer)), u32(0), u32(0))            // attach
	c.request(sid, 2, i32(0), i32(0), i32(int32(o.w)), i32(int32(o.h))) // damage
	c.request(sid, 6)                                                   // commit
}

func (c *conn) configureOverlay(outs []*output) {
	c.pointerID = c.allocID()
	c.request(idSeat, 0, u32(c.pointerID)) // wl_seat.get_pointer
	c.keyboardID = c.allocID()
	c.request(idSeat, 1, u32(c.keyboardID)) // wl_seat.get_keyboard

	for _, o := range outs {
		sid := c.newObj(o, roleSurface)
		c.request(idCompositor, 0, u32(sid)) // wl_compositor.create_surface

		lid := c.newObj(o, roleLayer)
		// get_layer_surface(id, surface, output, layer=overlay, namespace).
		c.request(idLayerShell, 0, u32(lid), u32(sid), u32(o.id(roleOutput)), u32(3), str("screenutil"))
		c.request(lid, 1, u32(1|2|4|8)) // set_anchor(top|bottom|left|right)
		c.request(lid, 2, i32(-1))      // set_exclusive_zone(-1)
		c.request(lid, 4, u32(1))       // set_keyboard_interactivity(exclusive)
		c.request(sid, 6)               // commit
	}

	for pending := len(outs); pending > 0; {
		id, op, body := c.read()
		i, role, ok := c.decode(id)
		if !ok || role != roleLayer {
			continue
		}

		switch op {
		case evLayerConfigure:
			serial := readU32(body, 0)
			c.request(id, 6, u32(serial))                                                      // ack_configure
			c.request(outs[i].id(roleSurface), 1, u32(outs[i].id(roleBuffer)), u32(0), u32(0)) // attach
			c.request(outs[i].id(roleSurface), 6)                                              // commit
			pending--
		case evLayerClosed:
			slog.Error("Compositor closed the overlay")
			os.Exit(1)
		}
	}
}

func (c *conn) fadeOut(outs []*output) {
	clock := outs[0]

	for shade := shadeDim + fadeStep; ; shade += fadeStep {
		last := shade >= shadeFull
		if last {
			shade = shadeFull
		}
		for _, o := range outs {
			dimAll(o.base, o.orig, shade)
			paintOutput(o, sel{})
			c.commitOutput(o, o == clock)
		}

		for { // block until the clock frame is presented
			id, op, _ := c.read()
			if i, role, ok := c.decode(id); ok && role == roleCallback && i == clock.idx && op == evDone {
				break
			}
		}
		if last {
			return
		}
	}
}

func (c *conn) selectRegion(outs []*output) (sel, bool) {
	clock := outs[0]
	bb := bounds(outs)

	var d drag
	shade := shadeFull
	for _, o := range outs {
		dimAll(o.base, o.orig, shade)
	}

	dirty := true
	framePending := false
	painted := sel{} // selection last committed, to know which outputs to clear

	tick := func() {
		fading := shade > shadeDim
		if fading {
			shade -= fadeStep
			if shade < shadeDim {
				shade = shadeDim
			}
			for _, o := range outs {
				dimAll(o.base, o.orig, shade)
			}
		}
		for _, o := range outs {
			if fading || o == clock || intersects(o, d.s) || intersects(o, painted) {
				paintOutput(o, d.s)
				c.commitOutput(o, o == clock)
			}
		}
		painted = d.s
		dirty = false
		framePending = true
	}
	tick()

	for {
		id, op, body := c.read()

		switch id {
		case c.pointerID:
			done, ok := c.pointer(&d, outs, bb, op, body)
			if d.active {
				dirty = true

				if !framePending { // callback loop stalled
					tick()
				}
			}
			if done {
				return d.s, ok
			}
		case c.keyboardID:
			if op != evKey {
				break
			}
			if done, ok := keyAction(body, !d.s.empty()); done {
				return d.s, ok
			}
		default:
			i, role, ok := c.decode(id)
			if !ok {
				break
			}
			switch role {
			case roleCallback:
				if op == evDone && i == clock.idx {
					framePending = false
					if dirty || shade > shadeDim {
						tick()
					}
				}
			case roleLayer:
				if op == evLayerClosed {
					slog.Error("Compositor closed the overlay")
					os.Exit(1)
				}
			}
		}
	}
}

// ----------------------------------------------------------------------------
// Encode
// ----------------------------------------------------------------------------

func regionPNG(outs []*output, s sel) []byte {
	w, h := s.x1-s.x0, s.y1-s.y0
	img := image.NewRGBA(image.Rect(0, 0, w, h))

	for _, o := range outs {
		gx0, gy0 := max(s.x0, o.x), max(s.y0, o.y)
		gx1, gy1 := min(s.x1, o.x+o.w), min(s.y1, o.y+o.h)
		if gx0 >= gx1 || gy0 >= gy1 {
			continue
		}

		for gy := gy0; gy < gy1; gy++ {
			row := (gy - o.y) * o.stride
			for gx := gx0; gx < gx1; gx++ {
				pixelBGRA(img, gx-s.x0, gy-s.y0, o.orig, row+(gx-o.x)*4, o.format == shmARGB)
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		slog.Error("Encode PNG", "err", err)
		os.Exit(1)
	}

	return buf.Bytes()
}

// Converts one native BGRA/BGRX pixel at src[o] into img at (x,y)
func pixelBGRA(img *image.RGBA, x, y int, src []byte, o int, hasAlpha bool) {
	a := byte(255)
	if hasAlpha {
		a = src[o+3]
	}
	i := img.PixOffset(x, y)
	img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = src[o+2], src[o+1], src[o], a
}

// Makes a bottom-up frame upright
func flipRows(mem []byte, stride, h int) {
	tmp := make([]byte, stride)
	for y := 0; y < h/2; y++ {
		top := mem[y*stride : (y+1)*stride]
		bot := mem[(h-1-y)*stride : (h-y)*stride]
		copy(tmp, top)
		copy(top, bot)
		copy(bot, tmp)
	}
}

// ----------------------------------------------------------------------------
// Clipboard
// ----------------------------------------------------------------------------

func (c *conn) serveClipboard(pngBytes []byte) {
	c.dataDevID = c.allocID()
	c.request(idDataCtl, 1, u32(c.dataDevID), u32(idSeat)) // get_data_device
	c.sourceID = c.allocID()
	c.request(idDataCtl, 0, u32(c.sourceID))   // create_data_source
	c.request(c.sourceID, 0, str("image/png")) // source.offer
	c.request(c.dataDevID, 0, u32(c.sourceID)) // device.set_selection

	for {
		id, op, _ := c.read()

		if id != c.sourceID {
			continue
		}

		switch op {
		case evSend:
			c.sendClipboard(pngBytes)
		case evCancelled: // someone else took the clipboard
			return
		}
	}
}

func (c *conn) sendClipboard(pngBytes []byte) {
	if len(c.fds) == 0 {
		return
	}

	fd := c.fds[0]
	c.fds = c.fds[1:]

	out := os.NewFile(uintptr(fd), "clip")
	if _, err := out.Write(pngBytes); err != nil {
		slog.Error("Write clipboard data", "err", err)
	}
	out.Close()
}

// ----------------------------------------------------------------------------
// Main
// ----------------------------------------------------------------------------

func socketPath() string {
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		slog.Error("XDG_RUNTIME_DIR not set")
		os.Exit(1)
	}

	disp := os.Getenv("WAYLAND_DISPLAY")
	if disp == "" {
		disp = "wayland-0"
	}

	if filepath.IsAbs(disp) {
		return disp
	}
	return filepath.Join(runtime, disp)
}

func dial() *conn {
	sock := socketPath()

	uc, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		slog.Error("Connect to wayland socket", "socket", sock, "err", err)
		os.Exit(1)
	}

	return &conn{uc: uc, nextID: idDynamic, objs: map[uint32]objRef{}}
}

func (c *conn) setup() ([]*output, uint32) {
	c.request(idDisplay, 1, u32(idRegistry)) // wl_display.get_registry
	c.request(idDisplay, 0, u32(idSync))     // wl_display.sync

	globals := map[string][2]uint32{}
	var outNames []uint32

	for {
		id, op, body := c.read()

		if id == idRegistry && op == 0 { // global
			name := readU32(body, 0)
			iface, off := readStr(body, 4)
			ver := readU32(body, off)

			if iface == "wl_output" {
				outNames = append(outNames, name)
			} else if _, seen := globals[iface]; !seen {
				globals[iface] = [2]uint32{name, ver}
			}
		} else if id == idSync && op == 0 { // callback.done
			break
		}
	}

	bind := func(iface string, newID, cap uint32) uint32 {
		global, ok := globals[iface]
		if !ok {
			slog.Error("Required wayland global missing", "interface", iface)
			os.Exit(1)
		}

		ver := min(global[1], cap)
		c.request(idRegistry, 0, u32(global[0]), newIDArg(iface, ver, newID))

		return ver
	}
	bind("wl_shm", idShm, 1)
	scVer := bind("zwlr_screencopy_manager_v1", idScreencopy, 3)
	bind("ext_data_control_manager_v1", idDataCtl, 1)
	bind("wl_seat", idSeat, 1)
	bind("wl_compositor", idCompositor, 4)
	bind("zwlr_layer_shell_v1", idLayerShell, 1)
	bind("zxdg_output_manager_v1", idXdgOutput, 3)

	if len(outNames) == 0 {
		slog.Error("No wl_output advertised")
		os.Exit(1)
	}

	outs := make([]*output, len(outNames))
	for i, name := range outNames {
		outs[i] = &output{name: name, idx: i}
		oid := c.newObj(outs[i], roleOutput)
		c.request(idRegistry, 0, u32(name), newIDArg("wl_output", 1, oid))

		xid := c.newObj(outs[i], roleXdgOutput)
		c.request(idXdgOutput, 1, u32(xid), u32(oid)) // get_xdg_output
	}

	c.request(idDisplay, 0, u32(idSync))
	for {
		id, op, body := c.read()
		if id == idSync && op == 0 {
			break
		}
		i, role, ok := c.decode(id)
		if !ok {
			continue
		}
		switch role {
		case roleOutput:
			c.readOutputGeometry(outs[i], op, body)
		case roleXdgOutput:
			if op == evLogicalPosition {
				outs[i].x = int(int32(readU32(body, 0)))
				outs[i].y = int(int32(readU32(body, 4)))
			}
		}
	}

	return outs, scVer
}

func (c *conn) readOutputGeometry(o *output, op uint32, body []byte) {
	if op == 1 && readU32(body, 0)&1 != 0 { // mode, current
		o.w = int(int32(readU32(body, 4)))
		o.h = int(int32(readU32(body, 8)))
	}
}

func (c *conn) shmBuffer(o *output, w, h, stride, format uint32) ([]byte, *os.File) {
	size := int(stride * h)

	f, err := os.CreateTemp(os.Getenv("XDG_RUNTIME_DIR"), "screenutil-*")
	if err != nil {
		slog.Error("Create shm temp file", "err", err)
		os.Exit(1)
	}
	os.Remove(f.Name()) // fd keeps it alive anyways

	if err := f.Truncate(int64(size)); err != nil {
		slog.Error("Size shm file", "err", err)
		os.Exit(1)
	}

	mem, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		slog.Error("Mmap shm file", "err", err)
		os.Exit(1)
	}

	poolID := c.newObj(o, rolePool)
	c.requestFD(idShm, 0, int(f.Fd()), u32(poolID), u32(uint32(size)))

	bufID := c.newObj(o, roleBuffer)
	c.request(poolID, 0, u32(bufID), u32(0), u32(w), u32(h), u32(stride), u32(format))

	return mem, f
}

func main() {
	c := dial()
	defer c.uc.Close()

	outs, scVer := c.setup()
	c.captureAll(outs, scVer)

	c.configureOverlay(outs)
	region, confirmed := c.selectRegion(outs)

	c.fadeOut(outs) // ease the overlay back to the desktop

	for _, o := range outs {
		c.request(o.id(roleLayer), 7)   // zwlr_layer_surface_v1.destroy
		c.request(o.id(roleSurface), 0) // wl_surface.destroy
		syscall.Munmap(o.mem)
		o.file.Close()
	}

	if !confirmed {
		slog.Info("cancelled")
		return
	}

	c.serveClipboard(regionPNG(outs, region))
}
