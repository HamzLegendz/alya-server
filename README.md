# alya-server

Dragonfly Minecraft Bedrock server dengan custom features:

- ✅ Anti-Cheat (Anti-Speed, Anti-Reach, Anti-Xray)
- ✅ Anti-Lag (GC tuning, chat spam protection)
- ✅ Log JOIN/QUIT detail (IP, device, OS, lokasi)
- ✅ Koordinat HUD aktif
- ✅ Difficulty Hard enforced
- ✅ Achievements enabled (patched gophertunnel)
- ✅ Spawn point: X=82, Y=30, Z=237
- ✅ Crash shield (recover dari panic)

## Dependency

Menggunakan fork gophertunnel yang di-patch:
- Repo: https://github.com/HamzLegendz/gophertunnel
- Patch: `AchievementsDisabled: false` di `minecraft/conn.go`

## Jalankan

```bash
pm2 start alya-server --name vite
```
