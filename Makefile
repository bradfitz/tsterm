# Fyne's packaging tool is a tool dependency in go.mod, so "go tool fyne"
# runs the version pinned there. The app's details are flags, not a
# FyneApp.toml, which the tool would rewrite, bumping its build number, on
# every run.
FYNE_PACKAGE = go tool fyne package -release -name tsterm \
	-app-id io.github.bradfitz.tsterm -icon Icon.png -app-version 0.1.0 -app-build 1

.PHONY: mac-app mac-install

# mac-app builds cmd/tsterm-gui/tsterm.app, the GUI as a macOS app, with its
# icon. It must run on a Mac.
mac-app:
	cd cmd/tsterm-gui && $(FYNE_PACKAGE) -os darwin

# mac-install installs tsterm.app in /Applications, replacing any there.
mac-install: mac-app
	rm -rf /Applications/tsterm.app
	cp -R cmd/tsterm-gui/tsterm.app /Applications/
	@echo "Installed /Applications/tsterm.app"
