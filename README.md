# GNOME Break Timer

A zero-bloat, CGO-free break-timer daemon for Linux desktop sessions (Fedora / GNOME / Wayland). It reminds you to take regular breaks (e.g., eye care, movement) and integrates perfectly with GNOME notifications and `zenity` for its UI menu.

## Features

- **Daemon-based**: Runs quietly in the background without a permanent GUI.
- **On-Demand Auto-Start**: Clicking the app icon to open the menu will automatically start the background daemon if it isn't already running.
- **Wayland / GNOME Native**: Uses DBus to communicate with GNOME notifications.
- **Configurable**: Define multiple timers (e.g. "20-20-20 rule", hourly stretch).
- **Interactive UI**: A built-in CLI menu powered by `zenity`.
- **Zero-Bloat**: Written in pure Go.

## Requirements

- `go` (to build the binary)
- `zenity` (for the GUI menu: `sudo dnf install zenity`)
- `paplay` (optional, for playing notification sounds - usually available by default via PulseAudio/PipeWire)

## Installation & Setup

We provide an interactive `setup.sh` script to automate the installation process across Linux distributions (like Fedora, Ubuntu, etc).

1. Clone the repository and navigate into it.
2. Run the setup script:

```bash
./setup.sh
```

The script will:
1. Compile the `gnome-break-timer` binary.
2. Install it to `~/.local/bin/`.
3. Ask if you want to create a **GNOME Application Menu (Grid) icon**.
4. Ask if you want to enable the **Auto-start feature** so the daemon runs on login.

### Manual Setup Steps (How it works under the hood)

If you prefer to set things up manually, here are the steps the script automates:

#### 1. GNOME Menu Grid Icon

To make the application show up in your GNOME App Grid (Application Menu), you need to create a `.desktop` entry file in `~/.local/share/applications/`.

Create a file named `~/.local/share/applications/gnome-break-timer.desktop` with the following content:

```ini
[Desktop Entry]
Name=GNOME Break Timer
Comment=Zero-bloat, CGO-free break-timer daemon
Exec=/home/YOUR_USERNAME/.local/bin/gnome-break-timer menu
Icon=preferences-system-time
Terminal=false
Type=Application
Categories=Utility;
```

*(Replace `/home/YOUR_USERNAME/` with the actual path to your home directory).*
Run `update-desktop-database ~/.local/share/applications` to refresh the menu immediately.

#### 2. Auto-start on System Boot

To automatically start the timer daemon in the background when you log into your desktop environment, create a `.desktop` file in the autostart directory (`~/.config/autostart/`).

Create a file named `~/.config/autostart/gnome-break-timer-daemon.desktop` with the following content:

```ini
[Desktop Entry]
Name=GNOME Break Timer Daemon
Comment=Zero-bloat, CGO-free break-timer daemon
Exec=/home/YOUR_USERNAME/.local/bin/gnome-break-timer daemon
Icon=preferences-system-time
Terminal=false
Type=Application
X-GNOME-Autostart-enabled=true
```

## Usage

If you used the setup script, you can search for **"GNOME Break Timer"** in your GNOME Activities / App Grid to launch the interactive menu. If the daemon isn't running yet, opening this menu will automatically start it in the background for you.

Alternatively, you can run commands from the terminal:

- **Start Daemon**: `gnome-break-timer daemon`
- **Open Menu**: `gnome-break-timer menu`
- **Pause all timers for 30m**: `gnome-break-timer pause 30m`
- **Resume all timers**: `gnome-break-timer resume`
- **View Status**: `gnome-break-timer status`

Configuration is saved in `~/.config/gnome-break-timer/config.json`.
