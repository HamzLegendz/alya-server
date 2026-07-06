package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/df-mc/dragonfly/server"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/cmd"
	"github.com/df-mc/dragonfly/server/item"
	"github.com/df-mc/dragonfly/server/player"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/go-gl/mathgl/mgl64"
	"github.com/pelletier/go-toml"
)

// ─────────────────────────────────────────────
//  LOG HELPERS
// ─────────────────────────────────────────────

const (
	tagJoin    = "JOIN"
	tagQuit    = "QUIT"
	tagWarn    = "WARN"
	tagCrash   = "CRASH"
	tagServer  = "SERVER"
	tagSecurity = "SECURITY"
)

func logf(tag, format string, args ...any) {
	loc := time.FixedZone("WIB", 7*3600)
	ts := time.Now().In(loc).Format("2006-01-02 15:04:05 WIB")
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("[%s] [%s] %s\n", ts, tag, msg)
}

func separator() {
	fmt.Println(strings.Repeat("─", 72))
}

// ─────────────────────────────────────────────
//  FILTERED SLOG HANDLER
//  Suppress benign Dragonfly world-load warnings
// ─────────────────────────────────────────────

type FilteredHandler struct{ slog.Handler }

func (h FilteredHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.Handler.Enabled(ctx, level)
}

func (h FilteredHandler) Handle(ctx context.Context, r slog.Record) error {
	switch r.Message {
	case "read column: unknown entity type",
		"read column: no block with runtime ID":
		return nil
	}
	return h.Handler.Handle(ctx, r)
}

func (h FilteredHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return FilteredHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h FilteredHandler) WithGroup(name string) slog.Handler {
	return FilteredHandler{Handler: h.Handler.WithGroup(name)}
}

// ─────────────────────────────────────────────
//  IP GEOLOCATION
// ─────────────────────────────────────────────

type ipInfo struct {
	Status     string `json:"status"`
	Country    string `json:"country"`
	RegionName string `json:"regionName"`
	City       string `json:"city"`
	Isp        string `json:"isp"`
}

func getIPLocation(ip string) string {
	if ip == "127.0.0.1" || ip == "::1" || ip == "localhost" {
		return "Localhost"
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://ip-api.com/json/" + ip + "?fields=status,country,regionName,city,isp")
	if err != nil {
		return "Unknown (Timeout)"
	}
	defer resp.Body.Close()

	var info ipInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || info.Status != "success" {
		return "Unknown"
	}
	return fmt.Sprintf("%s, %s, %s | ISP: %s", info.City, info.RegionName, info.Country, info.Isp)
}

// ─────────────────────────────────────────────
//  OS NAME MAPPER
// ─────────────────────────────────────────────

func mapOS(os int) string {
	names := map[int]string{
		1:  "Android",
		2:  "iOS",
		3:  "macOS",
		4:  "FireOS",
		5:  "GearVR",
		6:  "HoloLens",
		7:  "Windows 10/11",
		8:  "Win32",
		9:  "Dedicated Server",
		10: "AppleTV",
		11: "PlayStation",
		12: "Nintendo Switch",
		13: "Xbox",
		14: "Windows Phone",
	}
	if name, ok := names[os]; ok {
		return name
	}
	return fmt.Sprintf("Unknown (%d)", os)
}

// ─────────────────────────────────────────────
//  PLAYER IP HELPER
// ─────────────────────────────────────────────

func playerIP(p *player.Player) string {
	if p.Addr() == nil {
		return "N/A"
	}
	host, _, err := net.SplitHostPort(p.Addr().String())
	if err != nil {
		return p.Addr().String()
	}
	return host
}

// ─────────────────────────────────────────────
//  SECURITY HANDLER
//  Anti-Lag · Anti-Cheat · Anti-Xray
// ─────────────────────────────────────────────

type SecurityHandler struct {
	player.NopHandler
	p          *player.Player
	stoneMined int
	oreMined   int
}

// HandleChat — blokir pesan terlalu panjang (anti-spam/lag)
func (h *SecurityHandler) HandleChat(ctx *player.Context, message *string) {
	if len(*message) > 256 {
		ctx.Cancel()
		h.p.Message("§c[Anti-Lag] Pesan terlalu panjang, diblokir.")
		logf(tagSecurity, "Spam chat diblokir | Player: %s | Panjang: %d karakter", h.p.Name(), len(*message))
	}
}

// HandleMove — tolak gerakan melampaui batas kecepatan (anti-speed)
func (h *SecurityHandler) HandleMove(ctx *player.Context, newPos mgl64.Vec3, newRot cube.Rotation) {
	oldPos := h.p.Position()
	dx := newPos.X() - oldPos.X()
	dz := newPos.Z() - oldPos.Z()
	distSq := dx*dx + dz*dz

	// >10 blok per packet = speed hack (kecuali creative/spectator)
	if distSq > 100 && !h.p.GameMode().AllowsFlying() {
		ctx.Cancel()
		h.p.Message("§c[Anti-Cheat] Gerakan ditolak: kecepatan melebihi batas.")
		logf(tagSecurity, "Speed hack terdeteksi | Player: %s | Jarak: %.1f blok", h.p.Name(), math.Sqrt(distSq))
	}
}

// HandleAttackEntity — tolak serangan dari jarak terlalu jauh (anti-reach)
func (h *SecurityHandler) HandleAttackEntity(ctx *player.Context, e world.Entity, force, height *float64, critical *bool) {
	pp := h.p.Position()
	ep := e.Position()
	dx, dy, dz := ep.X()-pp.X(), ep.Y()-pp.Y(), ep.Z()-pp.Z()
	distSq := dx*dx + dy*dy + dz*dz

	// >6 blok = reach hack
	if distSq > 36 && !h.p.GameMode().AllowsFlying() {
		ctx.Cancel()
		h.p.Message("§c[Anti-Cheat] Serangan dibatalkan: target terlalu jauh.")
		logf(tagSecurity, "Reach hack terdeteksi | Player: %s | Jarak: %.2f blok", h.p.Name(), math.Sqrt(distSq))
	}
}

// HandleBlockBreak — anti-xray via rasio ore/block yang ditambang
// PENTING: Jangan panggil h.p.Tx() di dalam handler — Tx sudah ditutup saat handler dipanggil.
func (h *SecurityHandler) HandleBlockBreak(ctx *player.Context, pos cube.Pos, drops *[]item.Stack, xp *int) {
	// Hitung blok yang ditambang di kedalaman xray-zone (Y < 16)
	if pos.Y() < 16 {
		h.stoneMined++
	}

	// Deteksi ore dari item drop (safe, tanpa akses Tx)
	if drops == nil {
		return
	}
	for _, drop := range *drops {
		n, _ := drop.Item().EncodeItem()
		isRare := n == "minecraft:diamond" ||
			n == "minecraft:emerald" ||
			n == "minecraft:gold_ore" ||
			n == "minecraft:deepslate_diamond_ore" ||
			n == "minecraft:deepslate_gold_ore" ||
			n == "minecraft:deepslate_emerald_ore"

		if !isRare {
			continue
		}
		h.oreMined++
		ratio := float64(h.oreMined) / float64(h.stoneMined+1)
		if h.oreMined > 3 && ratio > 0.4 && !h.p.GameMode().AllowsFlying() {
			ctx.Cancel()
			h.p.Message("§c[Anti-Cheat] Pola tambang mencurigakan terdeteksi.")
			logf(tagSecurity, "X-ray terdeteksi | Player: %s | Rasio: %.2f | Ore: %d | Block: %d",
				h.p.Name(), ratio, h.oreMined, h.stoneMined)
		}
		break
	}
}

// HandleQuit — log detail saat player keluar
func (h *SecurityHandler) HandleQuit(p *player.Player) {
	ip := playerIP(p)
	deviceOS := "Unknown"
	deviceModel := "Unknown"

	if sess := p.Data().Session; sess != nil {
		cd := sess.ClientData()
		deviceOS = mapOS(int(cd.DeviceOS))
		deviceModel = cd.DeviceModel
	}

	separator()
	logf(tagQuit, "Player   : %s", p.Name())
	logf(tagQuit, "UUID     : %s", p.UUID())
	logf(tagQuit, "IP       : %s", ip)
	logf(tagQuit, "Device   : %s (%s)", deviceModel, deviceOS)
	logf(tagQuit, "Latency  : %v", p.Latency())
	logf(tagQuit, "Alasan   : Client Disconnected")
	separator()
}

// ─────────────────────────────────────────────
//  COMMANDS
// ─────────────────────────────────────────────

const hostUUID = "425d83b1-0e0d-4ea0-ab06-e43471711654"

// /gm — ganti gamemode (hanya host)
type GamemodeCommand struct {
	Mode string `cmd:"mode"`
}

func (c GamemodeCommand) Run(src cmd.Source, o *cmd.Output, tx *world.Tx) {
	p, ok := src.(*player.Player)
	if !ok {
		o.Error("Hanya bisa dijalankan oleh player.")
		return
	}
	if p.UUID().String() != hostUUID {
		o.Error("Kamu tidak memiliki izin untuk command ini.")
		return
	}

	modeMap := map[string]int{
		"survival": 0, "s": 0, "0": 0,
		"creative": 1, "c": 1, "1": 1,
		"adventure": 2, "a": 2, "2": 2,
		"spectator": 3, "sp": 3, "3": 3,
	}
	id, ok := modeMap[strings.ToLower(c.Mode)]
	if !ok {
		o.Error("Mode tidak valid. Gunakan: survival (s), creative (c), adventure (a), spectator (sp).")
		return
	}
	m, ok := world.GameModeByID(id)
	if !ok {
		o.Error("Gagal mengambil gamemode.")
		return
	}
	p.SetGameMode(m)
	o.Print(fmt.Sprintf("§aGamemode diubah ke §l%s§r§a.", c.Mode))
	logf(tagServer, "Gamemode diubah | Player: %s | Mode: %s", p.Name(), c.Mode)
}

// /ping — cek latency
type PingCommand struct{}

func (PingCommand) Run(src cmd.Source, o *cmd.Output, tx *world.Tx) {
	p, ok := src.(*player.Player)
	if !ok {
		o.Error("Hanya bisa dijalankan oleh player.")
		return
	}
	o.Print(fmt.Sprintf("§bPing kamu: §l%v§r§b ke server.", p.Latency()))
}

// ─────────────────────────────────────────────
//  MAIN
// ─────────────────────────────────────────────

func main() {
	// Anti-Lag: turunkan GC threshold agar memori tetap kecil
	debug.SetGCPercent(50)

	// Crash Shield: recover panic di main goroutine agar server tidak mati total
	defer func() {
		if r := recover(); r != nil {
			separator()
			logf(tagCrash, "PANIC di main goroutine: %v", r)
			debug.PrintStack()
			separator()
		}
	}()

	// Setup filtered logger — sembunyikan warning Dragonfly yang tidak kritis
	baseHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})
	slog.SetDefault(slog.New(FilteredHandler{Handler: baseHandler}))

	// Daftarkan command
	cmd.Register(cmd.New("gm", "Ubah gamemode (host only)", nil, GamemodeCommand{}))
	cmd.Register(cmd.New("ping", "Cek latency kamu ke server", nil, PingCommand{}))

	conf, err := readConfig(slog.Default())
	if err != nil {
		panic(err)
	}

	srv := conf.New()
	srv.CloseOnProgramEnd()

	// Paksa difficulty Hard & spawn point
	srv.World().SetDifficulty(world.DifficultyHard)
	srv.World().SetSpawn(cube.Pos{82, 30, 237})

	separator()
	logf(tagServer, "Vite Minecraft Server dimulai")
	logf(tagServer, "Port    : 19132 (UDP)")
	logf(tagServer, "Mode    : Hard Survival")
	logf(tagServer, "Spawn   : X=82, Y=30, Z=237")
	separator()

	srv.Listen()

	for p := range srv.Accept() {
		go handlePlayer(p)
	}
}

// handlePlayer menangani event satu player secara goroutine terpisah
func handlePlayer(p *player.Player) {
	// Crash Shield per-player — player lain tetap online jika satu goroutine panic
	defer func() {
		if r := recover(); r != nil {
			separator()
			logf(tagCrash, "PANIC goroutine player [%s]: %v", p.Name(), r)
			debug.PrintStack()
			separator()
		}
	}()

	// Ambil info client
	var deviceOS, deviceModel, gameVersion string
	if sess := p.Data().Session; sess != nil {
		cd := sess.ClientData()
		deviceOS = mapOS(int(cd.DeviceOS))
		deviceModel = cd.DeviceModel
		gameVersion = cd.GameVersion
	}

	ip := playerIP(p)

	// Fetch lokasi IP secara async sudah di goroutine ini, tidak memblokir server
	location := getIPLocation(ip)

	// Log JOIN detail
	separator()
	logf(tagJoin, "Player   : %s", p.Name())
	logf(tagJoin, "UUID     : %s", p.UUID())
	logf(tagJoin, "IP       : %s", ip)
	logf(tagJoin, "Lokasi   : %s", location)
	logf(tagJoin, "Device   : %s (%s)", deviceModel, deviceOS)
	logf(tagJoin, "Versi MC : %s", gameVersion)
	separator()

	// Delay kecil agar client siap menerima packet game rule
	time.Sleep(500 * time.Millisecond)

	// Aktifkan koordinat HUD
	p.ShowCoordinates()

	// Pasang security handler (anti-cheat, anti-lag, anti-xray)
	p.Handle(&SecurityHandler{p: p})
}

// ─────────────────────────────────────────────
//  CONFIG READER
// ─────────────────────────────────────────────

func readConfig(log *slog.Logger) (server.Config, error) {
	c := server.DefaultConfig()
	var zero server.Config

	if _, err := os.Stat("config.toml"); os.IsNotExist(err) {
		c.Server.Name = "Vite"
		c.Players.MaxCount = 10
		c.Network.Address = "0.0.0.0:19132"
		c.World.Folder = "world"
		c.Players.Folder = "players"
		c.Players.MaximumChunkRadius = 8

		data, err := toml.Marshal(c)
		if err != nil {
			return zero, fmt.Errorf("gagal encode config: %w", err)
		}
		if err := os.WriteFile("config.toml", data, 0644); err != nil {
			return zero, fmt.Errorf("gagal tulis config: %w", err)
		}
	}

	data, err := os.ReadFile("config.toml")
	if err != nil {
		return zero, fmt.Errorf("gagal baca config: %w", err)
	}
	if err := toml.Unmarshal(data, &c); err != nil {
		return zero, fmt.Errorf("gagal decode config: %w", err)
	}
	return c.Config(log)
}
