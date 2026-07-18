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
	// wl_display
	idDisplay = 1
	// Client owns everything from here
	idRegistry = 2
	idSync     = 3
	// These constants must be numbered in the exact order the objects are
	// created at runtime with no gaps
	idShm        = 4
	idOutput     = 5
	idScreencopy = 6
	idDataCtl    = 7
	idSeat       = 8
	idCompositor = 9
	idLayerShell = 10
	idFrame      = 11
	idPool       = 12
	idBuffer     = 13
	idPointer    = 14
	idKeyboard   = 15
	idSurface    = 16
	idLayerSurf  = 17
	idCallback   = 18
	idDataDev    = 19
	idSource     = 20
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
)

const (
	keyEnter     = 28
	keyKpEnter   = 96
	keyEscape    = 1
	statePressed = 1 // wl_keyboard key + wl_pointer button: pressed
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

// The timestamp of the last traced wire message
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

	if obj == idKeyboard && op == evKeymap {
		c.dropHeadFD()
	}

	return obj, op, body
}

// ----------------------------------------------------------------------------
// Capture
// ----------------------------------------------------------------------------

func (c *conn) captureParams(scVer uint32) (format, w, h, stride uint32) {
	for {
		id, op, body := c.read()

		if id == idFrame {
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

func (c *conn) waitReady() (yInvert bool) {
	for {
		id, op, body := c.read()

		if id == idFrame {
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

// ----------------------------------------------------------------------------
// Selection
// ----------------------------------------------------------------------------

type sel struct {
	x0, y0, x1, y1 int
}

func (s sel) empty() bool {
	return s.x0 >= s.x1 || s.y0 >= s.y1
}

func selFrom(ax, ay, bx, by, w, h int) sel {
	x0, x1 := order(ax, bx)
	y0, y1 := order(ay, by)

	return sel{clamp(x0, 0, w), clamp(y0, 0, h), clamp(x1, 0, w), clamp(y1, 0, h)}
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

func paintSelection(mem, base, orig []byte, stride, w, h int, s sel) {
	defer span("paintSelection")()
	copy(mem, base)
	if s.empty() {
		return
	}
	for y := s.y0; y < s.y1; y++ {
		row := y * stride
		copy(mem[row+s.x0*4:row+s.x1*4], orig[row+s.x0*4:row+s.x1*4])
	}
	borderDraw(mem, stride, w, h, s)
}

func borderDraw(mem []byte, stride, w, h int, s sel) {
	top, bot := s.y0, min(s.y1, h)
	left, right := s.x0, min(s.x1, w)

	for y := top; y < bot; y++ {
		onEdge := y < top+borderW || y >= bot-borderW
		row := y * stride
		for x := left; x < right; x++ {
			if onEdge || x < left+borderW || x >= right-borderW {
				borderPixel(mem, row+x*4)
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
	// Anchor set on button down
	ax, ay int
	// Last pointer position
	lx, ly int
	active bool
}

func (d *drag) pointer(op uint32, body []byte, w, h int) (done, confirmed bool) {
	switch op {
	case evEnter:
		d.lx, d.ly = fixedXY(body, 8)
	case evMotion:
		d.lx, d.ly = fixedXY(body, 4)

		if d.active {
			d.s = selFrom(d.ax, d.ay, d.lx, d.ly, w, h)
		}
	case evButton:
		return d.press(readU32(body, 12) == statePressed)
	}

	return false, false
}

func (d *drag) press(down bool) (done, confirmed bool) {
	if down {
		d.ax, d.ay = d.lx, d.ly
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

func (c *conn) redraw(mem, base, orig []byte, w, h, stride int, s sel, more bool) {
	paintSelection(mem, base, orig, stride, w, h, s)

	if more {
		c.request(idSurface, 3, u32(idCallback)) // wl_surface.frame
	}
	c.request(idSurface, 1, u32(idBuffer), u32(0), u32(0))                // attach
	c.request(idSurface, 2, i32(0), i32(0), i32(int32(w)), i32(int32(h))) // damage
	c.request(idSurface, 6)                                               // commit
}

func (c *conn) fadeOut(mem, orig []byte, w, h, stride int) {
	base := make([]byte, len(orig))

	for shade := shadeDim + fadeStep; ; shade += fadeStep {
		last := shade >= shadeFull
		if last {
			shade = shadeFull
		}
		dimAll(base, orig, shade)
		c.redraw(mem, base, orig, w, h, stride, sel{}, true)

		for { // block until this frame's done, so it is actually on screen
			if id, op, _ := c.read(); id == idCallback && op == evDone {
				break
			}
		}
		if last {
			return
		}
	}
}

func (c *conn) configureOverlay() {
	c.request(idSeat, 0, u32(idPointer))  // wl_seat.get_pointer
	c.request(idSeat, 1, u32(idKeyboard)) // wl_seat.get_keyboard

	c.request(idCompositor, 0, u32(idSurface)) // wl_compositor.create_surface

	// get_layer_surface(id, surface, output=null, layer=overlay, namespace)
	c.request(idLayerShell, 0, u32(idLayerSurf), u32(idSurface), u32(0), u32(3), str("screenutil"))
	c.request(idLayerSurf, 1, u32(1|2|4|8)) // set_anchor(top|bottom|left|right)
	c.request(idLayerSurf, 2, i32(-1))      // set_exclusive_zone(-1)
	c.request(idLayerSurf, 4, u32(1))       // set_keyboard_interactivity(exclusive)
	c.request(idSurface, 6)                 // commit (triggers configure)

	for {
		id, op, body := c.read()
		if id == idLayerSurf && op == evLayerConfigure {
			serial := readU32(body, 0)
			c.request(idLayerSurf, 6, u32(serial))                 // ack_configure
			c.request(idSurface, 1, u32(idBuffer), u32(0), u32(0)) // attach
			c.request(idSurface, 6)                                // commit
			return
		}
		if id == idLayerSurf && op == evLayerClosed {
			slog.Error("Compositor closed the freeze overlay")
			os.Exit(1)
		}
	}
}

func (c *conn) selectRegion(orig, mem []byte, w, h, stride int) (sel, bool) {
	var d drag
	shade := shadeFull

	base := make([]byte, len(orig))
	dimAll(base, orig, shade)

	dirty := true         // needs a repaint this frame
	framePending := false // a frame callback is in flight

	requestFrame := func() {
		fading := shade > shadeDim
		if fading {
			shade -= fadeStep
			if shade < shadeDim {
				shade = shadeDim
			}
			dimAll(base, orig, shade)
		}
		c.redraw(mem, base, orig, w, h, stride, d.s, true)
		dirty = false
		framePending = true
	}
	requestFrame()

	for {
		id, op, body := c.read()

		switch id {
		case idCallback:
			if op != evDone {
				break
			}
			framePending = false

			if dirty || shade > shadeDim {
				requestFrame()
			}
		case idPointer:
			done, ok := d.pointer(op, body, w, h)
			if d.active {
				dirty = true

				if !framePending {
					requestFrame()
				}
			}
			if done {
				return d.s, ok
			}
		case idKeyboard:
			if op != evKey {
				break
			}
			if done, ok := keyAction(body, !d.s.empty()); done {
				return d.s, ok
			}
		case idLayerSurf:
			if op == evLayerClosed {
				slog.Error("Compositor closed the region overlay")
				os.Exit(1)
			}
		}
	}
}

// ----------------------------------------------------------------------------
// Encode
// ----------------------------------------------------------------------------

func regionPNG(orig []byte, stride int, s sel, format uint32) []byte {
	w, h := s.x1-s.x0, s.y1-s.y0
	img := image.NewRGBA(image.Rect(0, 0, w, h))

	for y := 0; y < h; y++ {
		row := (s.y0 + y) * stride
		for x := 0; x < w; x++ {
			pixelBGRA(img, x, y, orig, row+(s.x0+x)*4, format == shmARGB)
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
	c.request(idDataCtl, 1, u32(idDataDev), u32(idSeat)) // get_data_device
	c.request(idDataCtl, 0, u32(idSource))               // create_data_source
	c.request(idSource, 0, str("image/png"))             // source.offer
	c.request(idDataDev, 0, u32(idSource))               // device.set_selection

	for {
		id, op, _ := c.read()

		if id != idSource {
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

	return &conn{uc: uc}
}

func (c *conn) bindGlobals() (scVer uint32) {
	c.request(idDisplay, 1, u32(idRegistry)) // wl_display.get_registry
	c.request(idDisplay, 0, u32(idSync))     // wl_display.sync
	globals := map[string][2]uint32{}

	for {
		id, op, body := c.read()

		if id == idRegistry && op == 0 { // global
			name := readU32(body, 0)
			iface, off := readStr(body, 4)
			ver := readU32(body, off)

			if _, seen := globals[iface]; !seen {
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
	bind("wl_output", idOutput, 1)
	scVer = bind("zwlr_screencopy_manager_v1", idScreencopy, 3)
	bind("ext_data_control_manager_v1", idDataCtl, 1)
	bind("wl_seat", idSeat, 1)
	bind("wl_compositor", idCompositor, 4)
	bind("zwlr_layer_shell_v1", idLayerShell, 1)

	return scVer
}

func (c *conn) shmBuffer(w, h, stride, format uint32) ([]byte, *os.File) {
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

	c.requestFD(idShm, 0, int(f.Fd()), u32(idPool), u32(uint32(size)))
	c.request(idPool, 0, u32(idBuffer), u32(0), u32(w), u32(h), u32(stride), u32(format))

	return mem, f
}

func main() {
	c := dial()
	defer c.uc.Close()

	scVer := c.bindGlobals()

	// Capture the first output into a shared buffer
	c.request(idScreencopy, 0, u32(idFrame), u32(0), u32(idOutput))
	format, w, h, stride := c.captureParams(scVer)

	mem, f := c.shmBuffer(w, h, stride, format)
	c.request(idFrame, 0, u32(idBuffer)) // zwlr_screencopy_frame_v1.copy
	yInvert := c.waitReady()

	iw, ih, istride := int(w), int(h), int(stride)

	if yInvert {
		flipRows(mem, istride, ih)
	}

	orig := make([]byte, len(mem))
	copy(orig, mem)

	c.configureOverlay()
	region, confirmed := c.selectRegion(orig, mem, iw, ih, istride)

	c.fadeOut(mem, orig, iw, ih, istride) // ease the overlay back to the desktop

	c.request(idLayerSurf, 7) // zwlr_layer_surface_v1.destroy
	c.request(idSurface, 0)   // wl_surface.destroy
	syscall.Munmap(mem)
	f.Close()

	if !confirmed {
		slog.Info("cancelled")
		return
	}

	c.serveClipboard(regionPNG(orig, istride, region, format))
}
