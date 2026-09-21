# Arch

Build a package for `/usr/bin/handover-gmessages`:

```sh
cd packaging/arch/handover-gmessages-git
makepkg -si
export HANDOVER_GMESSAGES_HELPER=/usr/bin/handover-gmessages
```

Put that environment variable on the `handoverd` user service if Handover is
installed from pacman. Do not put Google cookies in the unit file.
