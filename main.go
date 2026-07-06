package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/df-mc/dragonfly/server"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/cmd"
	"github.com/df-mc/dragonfly/server/item"
	"github.com/df-mc/dragonfly/server/player"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/go-gl/mathgl/mgl64"
	"github.com/pelletier/go-toml"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"time"
)

// FilteredHandler filters out benign non-critical world entity loading errors from the console logs
type FilteredHandler struct {
	slog.Handler
}

func (h FilteredHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.Handler.Enabled(ctx, level)
}

func (h FilteredHandler) Handle(ctx context.Context, r slog.Record) error {
	// Filter out benign entity/block loading warnings that clutter console
	if r.Message == "read column: unknown entity type" || r.Message == "read column: no block with runtime ID" {
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

// IPInfo holds JSON structure for free IP-API geolocation service
type IPInfo struct {
	Status      string `json:"status"`
	Country     string `json:"country"`
	RegionName  string `json:"regionName"`
	City        string `json:"city"`
	Isp         string `json:"isp"`
}

// getIPLocation fetches geographical location details for a given IP address
func getIPLocation(ip string) string {
	if ip == "127.0.0.1" || ip == "::1" || ip == "localhost" {
		return "Localhost (Internal Access)"
	}

	// 2 seconds timeout to prevent blocking player join sequences on network delays
	client := http.Client{
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("http://ip-api.com/json/" + ip)
	if err != nil {
		return "Unknown Location (Timeout)"
	}
	defer resp.Body.Close()

	var info IPInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || info.Status != "success" {
		return "Unknown Location (Lookup Failure)"
	}

	return fmt.Sprintf("%s, %s, %s (ISP: %s)", info.City, info.RegionName, info.Country, info.Isp)
}

// mapOS maps numerical DeviceOS constants to human-readable names
func mapOS(os int) string {
	switch os {
	case 1:
		return "Android"
	case 2:
		return "iOS"
	case 3:
		return "macOS"
	case 4:
		return "FireOS"
	case 5:
		return "GearVR"
	case 6:
		return "HoloLens"
	case 7:
		return "Windows 10/11"
	case 8:
		return "Win32"
	case 9:
		return "Dedicated Server"
	case 10:
		return "AppleTV"
	case 11:
		return "PlayStation"
	case 12:
		return "Nintendo Switch"
	case 13:
		return "Xbox"
	case 14:
		return "Windows Phone"
	default:
		return fmt.Sprintf("Unknown (%d)", os)
	}
}

// SecurityHandler implements player events for Anti-Lag, Anti-Cheat, and Anti-Xray protection
type SecurityHandler struct {
	player.NopHandler
	p          *player.Player
	stoneMined int
	oreMined   int
}

// HandleChat prevents spam messages from lagging the chat and packet queues
func (h *SecurityHandler) HandleChat(ctx *player.Context, message *string) {
	if len(*message) > 256 {
		ctx.Cancel()
		h.p.Message("§c[Anti-Lag] Chat message blocked: too long.")
	}
}

// HandleMove rejects movement packets that attempt to teleport or fly at extreme speeds
func (h *SecurityHandler) HandleMove(ctx *player.Context, newPos mgl64.Vec3, newRot cube.Rotation) {
	oldPos := h.p.Position()
	dx := newPos.X() - oldPos.X()
	dz := newPos.Z() - oldPos.Z()
	horizontalDistanceSquared := dx*dx + dz*dz

	// Block any horizontal movement greater than 10 blocks in a single packet (100 distance squared)
	// unless they are in creative mode or spectator mode.
	if horizontalDistanceSquared > 100 && !h.p.GameMode().AllowsFlying() {
		ctx.Cancel()
		h.p.Message("§c[Anti-Cheat] Movement rejected: speed exceeds threshold.")
	}
}

// HandleAttackEntity prevents players from hitting entities from too far away (Reach Hack)
func (h *SecurityHandler) HandleAttackEntity(ctx *player.Context, e world.Entity, force, height *float64, critical *bool) {
	playerPos := h.p.Position()
	entityPos := e.Position()
	
	dx := entityPos.X() - playerPos.X()
	dy := entityPos.Y() - playerPos.Y()
	dz := entityPos.Z() - playerPos.Z()
	distanceSquared := dx*dx + dy*dy + dz*dz
	
	// Max reach in Bedrock Survival is roughly 4-5 blocks.
	// 36 distance squared is 6 blocks linear distance.
	if distanceSquared > 36 && !h.p.GameMode().AllowsFlying() {
		ctx.Cancel()
		h.p.Message("§c[Anti-Cheat] Attack cancelled: target out of reach.")
		fmt.Printf("[WARN] Player %s flagged for Reach Hack! Distance: %.2f blocks\n", h.p.Name(), math.Sqrt(distanceSquared))
	}
}

// HandleBlockBreak implements Anti-Xray checks using mining ratio analysis.
// IMPORTANT: Do NOT call h.p.Tx() inside any handler — the Tx is closed before handlers fire.
// We use only stateless ratio tracking which is panic-safe.
func (h *SecurityHandler) HandleBlockBreak(ctx *player.Context, pos cube.Pos, drops *[]item.Stack, xp *int) {
	// We cannot call Tx() here. Instead, count based on what's being mined.
	// The check is purely ratio-based: if player mines lots of rare ores with very little stone, flag them.
	
	// Track stone/common blocks being mined based on position depth heuristic
	// Depth below Y=16 qualifies as deep mining (potential xray territory)
	if pos.Y() < 16 {
		h.stoneMined++
	}

	// We can't safely get block name without Tx, so we track xp gain as proxy for ore mining.
	// Alternatively, use the drops list to detect what was mined.
	if drops != nil {
		for _, drop := range *drops {
			n, _ := drop.Item().EncodeItem()
			isRareDrop := n == "minecraft:diamond" || n == "minecraft:gold_ore" ||
				n == "minecraft:emerald" || n == "minecraft:deepslate_diamond_ore" ||
				n == "minecraft:deepslate_gold_ore" || n == "minecraft:deepslate_emerald_ore"
			if isRareDrop {
				h.oreMined++
				ratio := float64(h.oreMined) / float64(h.stoneMined+1)
				if h.oreMined > 3 && ratio > 0.4 && !h.p.GameMode().AllowsFlying() {
					ctx.Cancel()
					h.p.Message("§c[Anti-Cheat] Suspicious mining pattern detected.")
					fmt.Printf("[WARN] Player %s flagged: X-ray ratio %.2f (Ores: %d, Depth-blocks: %d)\n",
						h.p.Name(), ratio, h.oreMined, h.stoneMined)
				}
				break
			}
		}
	}
}

// HandleQuit logs when a player disconnects from the server
func (h *SecurityHandler) HandleQuit(p *player.Player) {
	var deviceOS, deviceModel, ipStr string
	deviceOS = "Unknown"
	deviceModel = "Unknown"

	if p.Addr() != nil {
		host, _, err := net.SplitHostPort(p.Addr().String())
		if err == nil {
			ipStr = host
		} else {
			ipStr = p.Addr().String()
		}
	}

	// Try to get device info safely
	if sess := p.Data().Session; sess != nil {
		cd := sess.ClientData()
		deviceOS = mapOS(int(cd.DeviceOS))
		deviceModel = cd.DeviceModel
	}

	fmt.Printf("[QUIT] Player: %s | IP: %s | Device: %s | OS: %s (Client Disconnected)\n", p.Name(), ipStr, deviceModel, deviceOS)
}

// Gamemode Command: Changes player's gamemode (Only the Host has permissions to execute)
type GamemodeCommand struct {
	Mode string `cmd:"mode"`
}
func (c GamemodeCommand) Run(src cmd.Source, o *cmd.Output, tx *world.Tx) {
	p, ok := src.(*player.Player)
	if !ok {
		o.Error("This command can only be run by a player.")
		return
	}

	// Security: Restrict command access exclusively to the Host player's UUID
	if p.UUID().String() != "425d83b1-0e0d-4ea0-ab06-e43471711654" {
		o.Error("You do not have permission to execute this command.")
		return
	}

	var modeID int
	switch c.Mode {
	case "survival", "s", "0":
		modeID = 0
	case "creative", "c", "1":
		modeID = 1
	case "adventure", "a", "2":
		modeID = 2
	case "spectator", "sp", "3":
		modeID = 3
	default:
		o.Error("Invalid gamemode! Use survival (s), creative (c), adventure (a), or spectator (sp).")
		return
	}

	m, ok := world.GameModeByID(modeID)
	if !ok {
		o.Error("Could not retrieve gamemode.")
		return
	}

	p.SetGameMode(m)
	o.Print(fmt.Sprintf("§aYour gamemode has been set to %s.", c.Mode))
}

// Ping Command: Checks player's latency (Available to all players)
type PingCommand struct{}
func (PingCommand) Run(src cmd.Source, o *cmd.Output, tx *world.Tx) {
	p, ok := src.(*player.Player)
	if !ok {
		o.Error("This command can only be run by a player.")
		return
	}
	latency := p.Latency()
	o.Print(fmt.Sprintf("§bYour current latency: §l%v", latency))
}

func main() {
	// Anti-Lag: Aggressively collect garbage to keep memory footprint tiny and prevent GC spikes
	debug.SetGCPercent(50)

	// Recover from main loop crashes (Anti-Mokad / Server Crash Shield)
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[CRITICAL CRASH PREVENTED] Server panic recovered: %v\n", r)
			debug.PrintStack()
		}
	}()

	// Set up our custom filtered log handler to ignore benign entity warning/errors at LevelWarn
	baseHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(FilteredHandler{Handler: baseHandler})
	slog.SetDefault(logger)

	// Register custom commands (No cheat commands like /heal, maintaining hard survival integrity)
	cmd.Register(cmd.New("gm", "Change your gamemode", nil, GamemodeCommand{}))
	cmd.Register(cmd.New("ping", "Check your latency to the server", nil, PingCommand{}))

	conf, err := readConfig(slog.Default())
	if err != nil {
		panic(err)
	}

	srv := conf.New()
	srv.CloseOnProgramEnd()

	// Enforce World Difficulty to Hard (3) on Server Side
	srv.World().SetDifficulty(world.DifficultyHard)

	// Set default spawn point in the world (X=82, Y=30, Z=237)
	srv.World().SetSpawn(cube.Pos{82, 30, 237})

	fmt.Println("Starting Dragonfly server on port 19132...")
	srv.Listen()
	
	for p := range srv.Accept() {
		// Recover individual player goroutine panic crashes to keep other players online (Anti-Mokad)
		go func(playerObj *player.Player) {
			defer func() {
				if r := recover(); r != nil {
					fmt.Printf("[CRITICAL ERROR] Recovered player %s goroutine panic: %v\n", playerObj.Name(), r)
					debug.PrintStack()
				}
			}()

			// Retrieve client information from the player session config data
			sess := playerObj.Data().Session
			var deviceOS, deviceModel, gameVersion, ipStr, ipLocation string
			if sess != nil {
				cd := sess.ClientData()
				deviceOS = mapOS(int(cd.DeviceOS))
				deviceModel = cd.DeviceModel
				gameVersion = cd.GameVersion
			}
			if playerObj.Addr() != nil {
				host, _, err := net.SplitHostPort(playerObj.Addr().String())
				if err == nil {
					ipStr = host
				} else {
					ipStr = playerObj.Addr().String()
				}
				// Fetch IP Geolocation dynamically (Simple & Detailed)
				ipLocation = getIPLocation(ipStr)
			}

			// Log player join with detailed client parameters (capturable in PM2 logs)
			fmt.Printf("[JOIN] Player: %s | UUID: %s | IP: %s (%s) | Device: %s | OS: %s | Game Version: %s\n",
				playerObj.Name(), playerObj.UUID(), ipStr, ipLocation, deviceModel, deviceOS, gameVersion)

			// Small delay to ensure client is fully initialized before sending game rules
			time.Sleep(500 * time.Millisecond)

			// Enable Vanilla coordinates display for the client (public API - safe)
			playerObj.ShowCoordinates()

			// SetDifficulty is enforced world-wide above (srv.World().SetDifficulty(world.DifficultyHard))
			// showdaysplayed requires internal packet API - not accessible externally in Dragonfly v0.10

			// Set our custom anti-lag, anti-cheat, and anti-xray handler
			playerObj.Handle(&SecurityHandler{p: playerObj})
		}(p)
	}
}

// readConfig reads the configuration from the config.toml file, or creates the
// file if it does not yet exist.
func readConfig(log *slog.Logger) (server.Config, error) {
	c := server.DefaultConfig()
	var zero server.Config
	if _, err := os.Stat("config.toml"); os.IsNotExist(err) {
		// Configure optimized defaults
		c.Server.Name = "Vite"
		c.Players.MaxCount = 10
		c.Network.Address = "0.0.0.0:19132"
		c.World.Folder = "world"
		c.Players.Folder = "players"
		c.Players.MaximumChunkRadius = 8
		
		data, err := toml.Marshal(c)
		if err != nil {
			return zero, fmt.Errorf("failed encoding default config: %w", err)
		}
		if err := os.WriteFile("config.toml", data, 0644); err != nil {
			return zero, fmt.Errorf("failed creating config: %w", err)
		}
	}
	data, err := os.ReadFile("config.toml")
	if err != nil {
		return zero, fmt.Errorf("failed reading config: %w", err)
	}
	if err := toml.Unmarshal(data, &c); err != nil {
		return zero, fmt.Errorf("failed decoding config: %w", err)
	}
	return c.Config(log)
}
