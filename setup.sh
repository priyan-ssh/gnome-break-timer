#!/bin/bash
set -e

echo "======================================"
echo "GNOME Break Timer Setup"
echo "======================================"
echo

# 1. Check if Go is installed
if ! command -v go &> /dev/null; then
    echo "Error: Go is not installed or not in PATH."
    echo "Please install Go to compile the application."
    exit 1
fi

# 2. Check if Zenity is installed
if ! command -v zenity &> /dev/null; then
    echo "Warning: Zenity is not installed."
    echo "The 'menu' interface requires zenity. You may want to run: sudo dnf install zenity"
fi

# 3. Build the binary
echo "Building gnome-break-timer..."
go build -o gnome-break-timer main.go
echo "Build successful."

# 4. Install the binary
INSTALL_DIR="$HOME/.local/bin"
mkdir -p "$INSTALL_DIR"
echo "Installing binary to $INSTALL_DIR..."
cp gnome-break-timer "$INSTALL_DIR/"
chmod +x "$INSTALL_DIR/gnome-break-timer"
echo "Installation complete."

# Ensure ~/.local/bin is in PATH
if [[ ":$PATH:" != *":$INSTALL_DIR:"* ]]; then
    echo
    echo "WARNING: $INSTALL_DIR is not in your PATH."
    echo "Please add it to your ~/.bashrc or ~/.zshrc:"
    echo "  export PATH=\"\$HOME/.local/bin:\$PATH\""
    echo
fi

# 5. GNOME App Menu (Grid Icon)
read -p "Do you want to add this to your GNOME Application Menu? (Y/n): " ADD_MENU
if [[ "$ADD_MENU" != "n" && "$ADD_MENU" != "N" ]]; then
    APP_DIR="$HOME/.local/share/applications"
    mkdir -p "$APP_DIR"
    DESKTOP_FILE="$APP_DIR/gnome-break-timer.desktop"
    
    cat > "$DESKTOP_FILE" << EOF
[Desktop Entry]
Name=GNOME Break Timer
Comment=Zero-bloat, CGO-free break-timer daemon
Exec=$INSTALL_DIR/gnome-break-timer menu
Icon=preferences-system-time
Terminal=false
Type=Application
Categories=Utility;
EOF
    echo "Created GNOME App Menu entry at $DESKTOP_FILE"
    
    # Update desktop database
    if command -v update-desktop-database &> /dev/null; then
        update-desktop-database "$APP_DIR"
    fi
fi

# 6. Autostart
read -p "Do you want to auto-start the daemon when you log in? (Y/n): " AUTOSTART
if [[ "$AUTOSTART" != "n" && "$AUTOSTART" != "N" ]]; then
    AUTOSTART_DIR="$HOME/.config/autostart"
    mkdir -p "$AUTOSTART_DIR"
    AUTOSTART_FILE="$AUTOSTART_DIR/gnome-break-timer-daemon.desktop"
    
    cat > "$AUTOSTART_FILE" << EOF
[Desktop Entry]
Name=GNOME Break Timer Daemon
Comment=Zero-bloat, CGO-free break-timer daemon
Exec=$INSTALL_DIR/gnome-break-timer daemon
Icon=preferences-system-time
Terminal=false
Type=Application
X-GNOME-Autostart-enabled=true
EOF
    echo "Created Autostart entry at $AUTOSTART_FILE"
fi

echo
echo "Setup finished!"
echo "To start the daemon manually, run: gnome-break-timer daemon"
echo "To open the menu, run: gnome-break-timer menu"
