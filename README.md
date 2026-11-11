# Immich Wallpaper Sync – Go

Lädt Bilder aus einem einzelnen Immich-Album auf deinen Linux-PC nach.
**Lokal werden keine Bilder gelöscht**, auch wenn sie aus dem Album entfernt werden
oder das Album leer ist. In Immich wird nichts verändert.

## Voraussetzungen

- Linux (mit systemd für die optionale automatische Aktualisierung)
- Immich >= 3.2 mit Filter-/Cursor-Such-API
- Go >= 1.22 zum Kompilieren; keine externen Go-Module erforderlich

Die API-Implementierung basiert auf Immich 3.3.1. Die Verbindung zur eigenen Instanz
wird bei der Einrichtung geprüft. Zum Ausführen einer fertigen Binary werden weder
Go noch Python benötigt.

## Kompilieren

Die Go-Dateien liegen direkt im Hauptverzeichnis des Repositorys. Dort ausführen:

```bash
go test ./...
go vet ./...
CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags='-s -w' -o immich-wallpaper-sync .
```

Das bereitgestellte ZIP enthält zusätzlich eine fertige Binary für Linux x86_64/amd64
und `SHA256SUMS`. Diese Binary wird durch `.gitignore` vom Repository ausgeschlossen.
Die Prüfsummen beziehen sich auf das Download-Paket und werden ebenfalls nicht eingecheckt.

## Ersteinrichtung

1. In Immich einen API-Key mit ausschließlich diesen Rechten erstellen:
   `album.read`, `asset.read`, `asset.download`.
2. Das Album „Hintergründe“ öffnen und die normale Album-URL kopieren,
   etwa `https://immich.example.de/albums/UUID`. Keine öffentliche Freigabe-URL verwenden.
3. Das Projekt wie oben kompilieren oder die fertige Linux-amd64-Binary aus dem ZIP verwenden.
   Im Projektverzeichnis bzw. entpackten Ordner ausführen:

```bash
chmod +x immich-wallpaper-sync
./immich-wallpaper-sync --setup
```

Der Assistent fragt Immich-URL, Album-URL oder ID, API-Key (unsichtbar) und
Zielordner ab. Voreinstellung: `~/Bilder/Hintergründe`.

Anschließend:

```bash
systemctl --user daemon-reload
~/.local/bin/immich-wallpaper-sync --dry-run
~/.local/bin/immich-wallpaper-sync
systemctl --user enable --now immich-wallpaper-sync.timer
```

### Wallpaper in DankMaterialShell (DMS)

In der Wallpaper-Auswahl auf das **Ordnersymbol → Settings / Einstellungen** klicken.
Unter **Folders / Ordner → Add / Hinzufügen** den Zielordner auswählen,
standardmäßig `~/Bilder/Hintergründe`. Danach lässt sich dieser Ordner über das
Ordnersymbol als Quelle auswählen.

Bei älteren DMS-Versionen kann stattdessen **Browse / Durchsuchen** angeboten werden:
Den Zielordner öffnen und ein Bild daraus auswählen. Menübezeichnungen und verfügbare
Optionen hängen von der installierten DMS-Version ab.

Niri- und DMS-Einstellungen werden vom Programm nicht geändert. Der Downloader kann
auch unabhängig von DMS mit anderen Wallpaper-Programmen verwendet werden.

## Von der Python-Version wechseln

Die vorhandene JSON-Konfiguration und der Download-Status sind kompatibel und
bleiben erhalten. Im neu entpackten Ordner ausführen:

```bash
systemctl --user stop immich-wallpaper-sync.timer
systemctl --user stop immich-wallpaper-sync.service
chmod +x immich-wallpaper-sync
./immich-wallpaper-sync --install
systemctl --user daemon-reload
~/.local/bin/immich-wallpaper-sync --dry-run
~/.local/bin/immich-wallpaper-sync
systemctl --user enable --now immich-wallpaper-sync.timer
```

`--install` übernimmt die Konfiguration, installiert die Binary und schreibt den
Service auf direkten Binary-Aufruf um. Das ist notwendig, weil der alte Service
noch `/usr/bin/python3` verwendet. `--setup` nicht erneut ausführen.
Die Befehle bei einem Fehler anhalten und zuerst den Fehler beheben.

## Verhalten

- Neue Bilder werden als Original heruntergeladen und anhand der Immich-SHA-1-Prüfsumme geprüft.
- Vorhandene, unveränderte Bilder werden nicht erneut geladen.
- Lokal geänderte Bilder, die weiterhin im Album sind, werden durch das Original ersetzt.
- Aus dem Album entfernte Bilder bleiben lokal liegen, auch wenn sie lokal verändert wurden.
- Alte Status-Einträge bleiben erhalten, damit wieder hinzugefügte Bilder erkannt werden.
- Videos werden übersprungen. HEIC und RAW werden als Original gespeichert;
  die Anzeige hängt von DMS bzw. dem Wallpaper-Programm ab. JPG/PNG/WebP sind unkomplizierter.
- Dateinamen bestehen aus der vollständigen Immich-ID plus Dateiendung, um Kollisionen zu verhindern.
- Fremde Dateien werden nicht überschrieben oder gelöscht. Bei Namenskollisionen wird abgebrochen.
- Downloads erfolgen über temporäre Dateien und werden erst nach Prüfung ins Ziel verschoben.
- Bei Netzwerk- oder API-Fehlern wird abgebrochen; fertige Downloads bleiben erhalten.
- HTTP-Weiterleitungen werden abgelehnt, damit der API-Key nicht weitergereicht wird.
  Die direkte Immich-URL verwenden. TLS-Zertifikate müssen gültig sein.
- Versteckte Statusdateien im Zielordner nicht entfernen und den Ordner für dieses Album reservieren.
- Der Download ersetzt kein vollständiges Immich-Backup.

## Dateien und Timer

- Binary: `~/.local/bin/immich-wallpaper-sync`
- Konfiguration: `~/.config/immich-wallpaper-sync/config.json` (Modus 600)
- Units: `~/.config/systemd/user/immich-wallpaper-sync.{service,timer}`
- Status: `.immich-wallpaper-sync.json` im Zielordner (mit der Python-Version kompatibel)

Der User-Timer läuft etwa zwei Minuten nach Start des User-systemd-Managers und
danach eine Stunde nach Ende des letzten Laufs. Kein root oder Linger erforderlich.

```bash
# Manuell synchronisieren
systemctl --user start immich-wallpaper-sync.service

# Meldungen ansehen
journalctl --user -u immich-wallpaper-sync.service -n 40 --no-pager

# Nächsten Lauf anzeigen
systemctl --user list-timers immich-wallpaper-sync.timer

# Ausschalten
systemctl --user disable --now immich-wallpaper-sync.timer

# Konfiguration ändern
vi ~/.config/immich-wallpaper-sync/config.json
```

Ein anderes Album oder einen anderen Server mit einem neuen Zielordner verwenden.
Ein Umbenennen des gleichen Albums ist unproblematisch, da seine ID verwendet wird.
Mit `--config /pfad/config.json` kann ein einzelner manueller Lauf eine andere
Konfiguration verwenden. Der installierte Timer nutzt stets den Standardpfad.

## Entwicklung und Tests

```bash
go test ./...
go vet ./...
```

Die Tests verwenden einen lokalen HTTP-Testserver und prüfen Downloads,
Prüfsummenfehler, Wiederholungen, Seitennavigation, unvollständige Ergebnisse,
unveränderte Dateien, Probeläufe, Videos, fremde Dateien, Symlinks, leere Alben,
alte Python-Statusdaten und die Migration der systemd-Unit. Weiterleitungen werden
nicht verfolgt; API-Aufrufe bestehen ausschließlich aus GET sowie POST auf die Such-API.
Ein Test gegen die persönliche Immich-Instanz ist damit nicht ersetzt.

API-Grundlage: [Immich 3.3.1 OpenAPI](https://github.com/immich-app/immich/blob/v3.3.1/open-api/immich-openapi-specs.json).

## Lizenz

Dieses Projekt steht unter der [MIT-Lizenz](LICENSE).
