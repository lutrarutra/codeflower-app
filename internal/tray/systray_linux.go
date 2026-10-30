package tray

import (
	"fyne.io/systray"

	"codeflower-app/assets"
)

const notifies = false

func run(menu Menu, status <-chan Status, _ <-chan Notice, done <-chan struct{}) error {
	systray.Run(func() {
		systray.SetIcon(assets.Icon)
		systray.SetTooltip(menu.Tooltip)
		systray.SetOnTapped(func() { go menu.Open() })
		open := systray.AddMenuItem("Open code flower", "Open the client in the browser")
		line := systray.AddMenuItem(menu.Tooltip, "")
		line.Disable()
		stopNow := systray.AddMenuItem("Stop now", "Stop the sandboxes without waiting for their chats")
		keepRunning := systray.AddMenuItem("Keep running", "Call the quit off")
		stopNow.Hide()
		keepRunning.Hide()
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit", "Stop the sandboxes started here and quit")
		go func() {
			for {
				select {
				case <-done:
					systray.Quit()
					return
				case current := <-status:
					line.SetTitle(current.Text)
					for _, item := range []*systray.MenuItem{stopNow, keepRunning} {
						if current.WrappingUp {
							item.Show()
						} else {
							item.Hide()
						}
					}
				case <-open.ClickedCh:
					go menu.Open()
				case <-stopNow.ClickedCh:
					go menu.StopNow()
				case <-keepRunning.ClickedCh:
					go menu.KeepRunning()
				case <-quit.ClickedCh:
					go menu.Quit()
				}
			}
		}()
	}, func() {})
	return nil
}
