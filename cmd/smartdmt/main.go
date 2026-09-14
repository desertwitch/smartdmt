/*
smartdmt is a small utility that provides a simple terminal user interface (TUI)
for inspecting your connected block devices SMART information.

It was developed primarily as a companion application to ShredOS, so that such
information can be observed during disk wiping. However, the utility also
functions as a standalone program.

The TUI acts as a visual wrapper, calling lsblk and smartctl under the hood to
provide a side-by-side view of all block devices and their SMART information.
*/
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	Version = "devel"

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("170"))

	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("62"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))
)

var _ list.Item = Disk{}

type Disk struct {
	Name   string `json:"-"`
	Path   string `json:"path"`
	Model  string `json:"model"`
	Serial string `json:"serial"`
}

func (d Disk) Title() string       { return d.Name }
func (d Disk) FilterValue() string { return d.Name + " " + d.Serial }
func (d Disk) Description() string { return truncate(d.Serial, 20) }

type (
	lsblkOutput struct {
		Blockdevices []Disk `json:"blockdevices"`
	}

	smartctlIdentOutput struct {
		ModelName    string `json:"model_name"`
		SerialNumber string `json:"serial_number"`
	}

	request struct {
		path      string
		tableOnly bool
	}

	model struct {
		shown   request // displayed
		loading request // in-flight

		tableOnly bool
		smartData string

		disksLoaded bool
		lastReload  time.Time

		width  int
		height int
		ready  bool

		list     list.Model
		viewport viewport.Model
		ctx      context.Context //nolint:containedctx
	}

	disksLoadedMsg []Disk

	smartDataMsg struct {
		req  request
		data string
		err  error
	}
)

func initialModel(ctx context.Context) model {
	delegate := list.NewDefaultDelegate()
	delegate.ShowDescription = true
	delegate.SetSpacing(0)

	l := list.New([]list.Item{}, delegate, 0, 0)
	l.Title = "Devices"
	l.SetShowStatusBar(true)
	l.SetShowHelp(false)
	l.SetFilteringEnabled(true)
	l.Styles.Title = titleStyle

	v := viewport.New(0, 0)

	return model{
		tableOnly: true,
		list:      l,
		viewport:  v,
		ctx:       ctx,
	}
}

func (m model) Init() tea.Cmd {
	return loadDisks(m.ctx)
}

func loadDisks(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		lsblkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		cmd := exec.CommandContext(lsblkCtx, "lsblk", "-d", "-o", "PATH,MODEL,SERIAL", "-n", "--json")
		output, _ := cmd.Output()
		cancel()

		var data lsblkOutput
		if err := json.Unmarshal(output, &data); err != nil {
			return disksLoadedMsg([]Disk{})
		}

		var disks []Disk
		for _, d := range data.Blockdevices {
			if d.Path == "" || strings.Contains(d.Path, "loop") || strings.Contains(d.Path, "ram") {
				continue
			}

			d.Name = filepath.Base(d.Path)

			// Prefer smartctl model/serial to lsblk, if available.
			if ident, err := smartctlIdent(ctx, d.Path); err == nil {
				if ident.ModelName != "" {
					d.Model = ident.ModelName
				}
				if ident.SerialNumber != "" {
					d.Serial = ident.SerialNumber
				}
			}

			disks = append(disks, d)
		}

		return disksLoadedMsg(disks)
	}
}

func smartctlIdent(ctx context.Context, diskPath string) (smartctlIdentOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "smartctl", "-i", "--json", diskPath)
	output, _ := cmd.Output()

	var ident smartctlIdentOutput
	if err := json.Unmarshal(output, &ident); err != nil {
		return smartctlIdentOutput{}, err //nolint:wrapcheck
	}

	return ident, nil
}

func loadSmartData(ctx context.Context, req request) tea.Cmd {
	return func() tea.Msg {
		flag := "-x"
		if req.tableOnly {
			flag = "-A"
		}

		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, "smartctl", flag, req.path) //nolint:gosec
		output, err := cmd.CombinedOutput()

		return smartDataMsg{
			req:  req,
			data: string(output),
			err:  err,
		}
	}
}

type dimensions struct {
	leftColWidth     int
	rightColWidth    int
	headerWidth      int
	panelHeight      int
	panelInnerHeight int
	listWidth        int
	listHeight       int
	viewportWidth    int
	viewportHeight   int
}

func calcDimensions(width, height int) dimensions {
	// Layout: 20/80 split with 2-char gap between panels
	// -4 for gap between panels, -2 for panel border (1 left + 1 right)
	leftColWidth := (width-4)*20/100 - 2
	rightColWidth := (width-4)*80/100 - 2

	// Header width: +4 for gap and borders
	headerWidth := leftColWidth + rightColWidth + 4

	// Panel height: total height minus header (3: content + border) and help bar (1) and newline (1)
	panelHeight := height - 5

	return dimensions{
		leftColWidth:  leftColWidth,
		rightColWidth: rightColWidth,

		headerWidth: headerWidth,

		panelHeight:      panelHeight,
		panelInnerHeight: panelHeight - 2, // minus border (top + bottom)

		// List: panel size minus border (2) for width, minus border (2) + chrome (2) for height
		listWidth:  leftColWidth - 2,
		listHeight: panelHeight - 4,

		// Viewport: panel size minus border (2) + padding (2) for width,
		// minus border (2) + title lines (2) + padding (2) for height
		viewportWidth:  rightColWidth - 4,
		viewportHeight: panelHeight - 6,
	}
}

//nolint:cyclop,funlen
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:ireturn
	var cmds []tea.Cmd
	var listCmd, vpCmd tea.Cmd
	var force bool

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit

		case "r", "R":
			cmds = append(cmds, loadDisks(m.ctx))
			force = true

		case "t", "T":
			m.tableOnly = !m.tableOnly
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		d := calcDimensions(m.width, m.height)
		m.list.SetSize(d.listWidth, d.listHeight)
		m.viewport.Width = d.viewportWidth
		m.viewport.Height = d.viewportHeight

		m.ready = true

	case disksLoadedMsg:
		m.disksLoaded = true

		// Filtering is reset on list update, so skip when filtered.
		if m.list.FilterState() != list.Unfiltered {
			break
		}

		// Save the currently selected disk
		var selectedPath string
		if disk, ok := m.selectedDisk(); ok {
			selectedPath = disk.Path
		}

		// Convert to list items
		items := make([]list.Item, len(msg))
		for i, disk := range msg {
			items[i] = disk
		}
		m.list.SetItems(items)

		// Try to keep the same disk selected
		if selectedPath != "" {
			for i, disk := range msg {
				if disk.Path == selectedPath {
					m.list.Select(i)

					break
				}
			}
		}

	case smartDataMsg:
		// Anything that isn't the request we're waiting on is stale.
		if msg.req != m.loading {
			break
		}

		// Same disk and same view mode: keep the scroll position.
		isReload := msg.req == m.shown
		savedOffset := m.viewport.YOffset

		m.smartData = msg.data
		if errors.Is(msg.err, context.DeadlineExceeded) {
			m.smartData = "Timed out; device may be unresponsive."
		}
		m.loading = request{}
		m.shown = msg.req

		// Wrap content around viewport width
		smartData := lipgloss.NewStyle().
			MarginLeft(1).
			Width(m.viewport.Width).
			Render(strings.TrimSuffix(m.smartData, "\n"))
		m.viewport.SetContent(smartData)

		// Preserve scroll position on reload, reset on disk change
		if isReload {
			m.viewport.SetYOffset(savedOffset)
		} else {
			m.viewport.GotoTop()
		}

		m.lastReload = time.Now()
	}

	m.list, listCmd = m.list.Update(msg)
	cmds = append(cmds, listCmd)

	m.viewport, vpCmd = m.viewport.Update(msg)
	cmds = append(cmds, vpCmd)

	// What the UI should be showing, after the list has had its turn.
	want := request{tableOnly: m.tableOnly}
	if disk, ok := m.selectedDisk(); ok {
		want.path = disk.Path
	}

	switch {
	case want.path == "":
		// No disk is selected anymore, clear the residual state.
		if m.shown.path != "" || m.loading.path != "" {
			m.shown, m.loading = request{}, request{}
			m.smartData = ""
			m.viewport.SetContent("")
		}

	case force || (want != m.shown && want != m.loading):
		cmds = append(cmds, loadSmartData(m.ctx, want))
		m.loading = want
	}

	return m, tea.Batch(cmds...)
}

func (m model) View() string {
	if !m.ready {
		return "Initializing..."
	}

	d := calcDimensions(m.width, m.height)

	// Header - Program information
	header := panelStyle.
		Width(d.headerWidth).
		Render(titleStyle.Render(fmt.Sprintf(" smartdmt %s - SMART Device Monitoring Terminal", Version)))

	// Left panel - Device list
	var leftContent string
	if !m.disksLoaded {
		leftContent = helpStyle.Render(" Scanning devices...")
	} else {
		leftContent = m.list.View()
	}

	leftPanel := panelStyle.
		Width(d.leftColWidth).
		Height(d.panelInnerHeight).
		Render(leftContent)

	// Right panel content - Smart data
	var rightContent strings.Builder
	if disk, ok := m.selectedDisk(); ok {
		mode := "Full View"
		if m.tableOnly {
			mode = "Table View"
		}
		rightContent.WriteString(titleStyle.Render(fmt.Sprintf(" %s [%s]\n Model: %s • Serial: %s",
			disk.Path, mode, truncate(disk.Model, 40), truncate(disk.Serial, 20))) + "\n\n")
	}

	// In-flight for something we aren't already displaying.
	if m.loading.path != "" && m.loading != m.shown {
		rightContent.WriteString(" Loading...")
	} else {
		rightContent.WriteString(m.viewport.View())
	}

	rightPanel := panelStyle.
		Width(d.rightColWidth).
		Height(d.panelInnerHeight).
		Render(rightContent.String())

	// Combine panels
	content := lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, "  ", rightPanel)

	// Help bar
	help := helpStyle.Render(" ↑/↓: navigate • /: filter • r: reload • t: toggle view • pgup/pgdn/mouse: scroll • q: quit")
	if !m.lastReload.IsZero() {
		help += helpStyle.Render(" | updated: " + m.lastReload.Format("15:04:05"))
	}

	return header + "\n" + content + "\n" + help
}

func (m model) selectedDisk() (Disk, bool) {
	if item := m.list.SelectedItem(); item != nil {
		if disk, ok := item.(Disk); ok {
			return disk, true
		}
	}

	return Disk{}, false
}

func truncate(s string, maxLen int) string {
	if s == "" {
		return "-"
	}
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}

	return s[:maxLen-3] + "..."
}

func main() {
	var exitCode int
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "panic: %v\n\n", r)
			debug.PrintStack()
			exitCode = 1
		}
		os.Exit(exitCode)
	}()

	fmt.Fprintf(os.Stderr, "smartdmt %s - SMART Device Monitoring Terminal\n", Version)
	fmt.Fprintf(os.Stderr, "https://github.com/desertwitch/smartdmt\n\n")

	for _, bin := range []string{"lsblk", "smartctl"} {
		if _, err := exec.LookPath(bin); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %s not available: %v\n", bin, err)
			exitCode = 1

			return
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := tea.NewProgram(
		initialModel(ctx),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithContext(ctx),
	)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		exitCode = 1

		return
	}
}
