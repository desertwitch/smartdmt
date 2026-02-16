package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
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

	lsblkRe = regexp.MustCompile(`PATH="([^"]*)" MODEL="([^"]*)" SERIAL="([^"]*)"`)

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("170"))

	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("62"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))
)

var _ list.Item = (*Disk)(nil)

type Disk struct {
	Name   string
	Path   string
	Model  string
	Serial string
}

func (d Disk) Title() string       { return d.Name }
func (d Disk) FilterValue() string { return d.Name + " " + d.Serial }
func (d Disk) Description() string { return truncate(d.Serial, 20) }

type (
	model struct {
		currentDisk string // displayed
		loadingDisk string // in-flight

		tableOnly  bool
		smartData  string
		smartError error

		lastReload time.Time

		width  int
		height int
		ready  bool

		list     list.Model
		viewport viewport.Model
		ctx      context.Context //nolint:containedctx
	}

	tickMsg        time.Time
	disksLoadedMsg []Disk

	smartDataMsg struct {
		diskPath  string
		data      string
		err       error
		tableOnly bool
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
	return tea.Batch(
		loadDisks(m.ctx),
		tickCmd(),
	)
}

func loadDisks(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		cmd := exec.CommandContext(ctx, "lsblk", "-d", "-o", "PATH,MODEL,SERIAL", "-n", "--pairs")
		output, err := cmd.Output()
		if err != nil {
			return disksLoadedMsg([]Disk{})
		}

		var disks []Disk
		lines := strings.SplitSeq(strings.TrimSpace(string(output)), "\n")
		for line := range lines {
			matches := lsblkRe.FindStringSubmatch(line)
			if matches == nil {
				continue
			}
			path := matches[1]
			if strings.Contains(path, "loop") || strings.Contains(path, "ram") {
				continue
			}
			disks = append(disks, Disk{
				Name:   strings.TrimPrefix(path, "/dev/"),
				Path:   path,
				Model:  matches[2],
				Serial: matches[3],
			})
		}

		return disksLoadedMsg(disks)
	}
}

func loadSmartData(ctx context.Context, diskPath string, tableOnly bool) tea.Cmd {
	return func() tea.Msg {
		flag := "-x"
		if tableOnly {
			flag = "-A"
		}

		cmd := exec.CommandContext(ctx, "smartctl", flag, diskPath)
		output, err := cmd.CombinedOutput()

		return smartDataMsg{
			diskPath:  diskPath,
			data:      string(output),
			err:       err,
			tableOnly: tableOnly,
		}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(30*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
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

//nolint:gocognit,gocyclo,cyclop,funlen
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:ireturn
	var cmds []tea.Cmd
	var listCmd, vpCmd tea.Cmd

	var selectedDisk *Disk
	if disk := m.selectedDisk(); disk != nil {
		selectedDisk = disk
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit

		case "r", "R":
			if selectedDisk != nil {
				cmds = append(cmds, loadSmartData(m.ctx, selectedDisk.Path, m.tableOnly))
				m.loadingDisk = selectedDisk.Path
			}

		case "t", "T":
			m.tableOnly = !m.tableOnly
			if selectedDisk != nil {
				m.currentDisk = "" // full reload
				cmds = append(cmds, loadSmartData(m.ctx, selectedDisk.Path, m.tableOnly))
				m.loadingDisk = selectedDisk.Path
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		d := calcDimensions(m.width, m.height)
		m.list.SetSize(d.listWidth, d.listHeight)
		m.viewport.Width = d.viewportWidth
		m.viewport.Height = d.viewportHeight

		if !m.ready {
			m.ready = true
			if selectedDisk != nil {
				cmds = append(cmds, loadSmartData(m.ctx, selectedDisk.Path, m.tableOnly))
				m.loadingDisk = selectedDisk.Path
			}
		}

	case disksLoadedMsg:
		// Filtering is reset on list update, so skip when filtered.
		if m.list.FilterState() != list.Unfiltered {
			break
		}

		// Convert to list items
		items := make([]list.Item, len(msg))
		for i, disk := range msg {
			items[i] = disk
		}
		m.list.SetItems(items)

		// Try to keep the same disk selected
		if selectedDisk != nil && selectedDisk.Path != "" {
			for i, disk := range msg {
				if disk.Path == selectedDisk.Path {
					m.list.Select(i)

					break
				}
			}
		}

		if len(msg) > 0 && m.ready && m.currentDisk == "" {
			if selectedDisk != nil {
				cmds = append(cmds, loadSmartData(m.ctx, selectedDisk.Path, m.tableOnly))
				m.loadingDisk = selectedDisk.Path
			}
		}

	case smartDataMsg:
		if selectedDisk != nil && msg.diskPath == selectedDisk.Path && msg.tableOnly == m.tableOnly {
			isReload := m.currentDisk == msg.diskPath
			savedOffset := m.viewport.YOffset

			m.smartData = msg.data
			m.smartError = msg.err
			m.loadingDisk = ""
			m.currentDisk = msg.diskPath

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

	case tickMsg:
		cmds = append(cmds, tickCmd())
		cmds = append(cmds, loadDisks(m.ctx))

		if selectedDisk != nil {
			cmds = append(cmds, loadSmartData(m.ctx, selectedDisk.Path, m.tableOnly))
			m.loadingDisk = selectedDisk.Path
		}
	}

	m.list, listCmd = m.list.Update(msg)
	cmds = append(cmds, listCmd)

	m.viewport, vpCmd = m.viewport.Update(msg)
	cmds = append(cmds, vpCmd)

	// Check if selection changed after list updated
	var prevPath string
	if selectedDisk != nil {
		prevPath = selectedDisk.Path
	}
	if selectedDisk := m.selectedDisk(); selectedDisk != nil {
		// Queue a data reload if the disk is different and we're not already loading it.
		if selectedDisk.Path != prevPath && selectedDisk.Path != m.loadingDisk {
			cmds = append(cmds, loadSmartData(m.ctx, selectedDisk.Path, m.tableOnly))
			m.loadingDisk = selectedDisk.Path
		}
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
	leftContent := m.list.View()
	leftPanel := panelStyle.
		Width(d.leftColWidth).
		Height(d.panelInnerHeight).
		Render(leftContent)

	// Right panel content - Smart data
	var rightContent strings.Builder
	if disk := m.selectedDisk(); disk != nil {
		mode := "Full View"
		if m.tableOnly {
			mode = "Table View"
		}
		rightContent.WriteString(titleStyle.Render(fmt.Sprintf(" %s [%s]\n Model: %s • Serial: %s",
			disk.Path, mode, truncate(disk.Model, 40), truncate(disk.Serial, 20))) + "\n\n")
	} else {
		rightContent.WriteString(titleStyle.Render(" No block devices found.") + "\n\n")
	}

	if m.loadingDisk != "" && m.currentDisk != m.loadingDisk {
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

func (m model) selectedDisk() *Disk {
	if item := m.list.SelectedItem(); item != nil {
		if disk, ok := item.(Disk); ok {
			return &disk
		}
	}

	return nil
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
	fmt.Fprintf(os.Stderr, "(c) Copyright 2026 - desertwitch / License: MIT License\n\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := exec.CommandContext(ctx, "lsblk", "-d", "-o", "NAME,MODEL,SERIAL", "-n", "-p").Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: lsblk not available or failed: %v\n", err)
		exitCode = 1

		return
	}

	if err := exec.CommandContext(ctx, "smartctl", "--version").Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: smartctl not available: %v\n", err)
		exitCode = 1

		return
	}

	p := tea.NewProgram(
		initialModel(ctx),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithContext(ctx),
		tea.WithoutCatchPanics(),
	)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		exitCode = 1

		return
	}
}
