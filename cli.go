package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// --- HTTP client helpers ---

func apiGetStatus(serverURL string) (StatusResponse, error) {
	resp, err := http.Get(serverURL + "/api/status")
	if err != nil {
		return StatusResponse{}, fmt.Errorf("reaching server: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return StatusResponse{}, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return StatusResponse{}, fmt.Errorf("server returned %d: %s", resp.StatusCode, string(body))
	}

	var status StatusResponse
	if err := json.Unmarshal(body, &status); err != nil {
		return StatusResponse{}, fmt.Errorf("parsing response: %w", err)
	}
	return status, nil
}

// apiPostTier hits POST /api/tier/{tier}/{action}.
func apiPostTier(serverURL string, tier int, action string) error {
	url := fmt.Sprintf("%s/api/tier/%d/%s", serverURL, tier, action)
	resp, err := http.Post(url, "", nil)
	if err != nil {
		return fmt.Errorf("reaching server: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusAccepted:
		return nil
	case http.StatusConflict:
		return fmt.Errorf("transition already in progress")
	case http.StatusNotFound:
		if isRouteNotFound(body) {
			return fmt.Errorf("server does not support %s — upgrade the homelab-manager binary on the server", url)
		}
		return fmt.Errorf("tier %d not found", tier)
	default:
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// isRouteNotFound returns true when a 404 body looks like Go's default mux
// response ("404 page not found"), which means the route isn't registered —
// as opposed to a handler-level JSON 404 that indicates a missing resource.
func isRouteNotFound(body []byte) bool {
	return strings.HasPrefix(strings.TrimSpace(string(body)), "404 page not found")
}

// apiPostNight hits POST /api/night/{action}.
func apiPostNight(serverURL, action string) error {
	url := fmt.Sprintf("%s/api/night/%s", serverURL, action)
	resp, err := http.Post(url, "", nil)
	if err != nil {
		return fmt.Errorf("reaching server: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusAccepted:
		return nil
	case http.StatusConflict:
		return fmt.Errorf("transition already in progress")
	case http.StatusBadRequest:
		return fmt.Errorf("night mode is not configured on the server (set night_mode.keep_awake_tags)")
	case http.StatusNotFound:
		if isRouteNotFound(body) {
			return fmt.Errorf("server does not support %s — upgrade the homelab-manager binary on the server", url)
		}
		return fmt.Errorf("server returned 404: %s", strings.TrimSpace(string(body)))
	default:
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// apiPostInstance hits POST /api/instance/{vmid}/{action}.
func apiPostInstance(serverURL string, vmid int, action string) error {
	url := fmt.Sprintf("%s/api/instance/%d/%s", serverURL, vmid, action)
	resp, err := http.Post(url, "", nil)
	if err != nil {
		return fmt.Errorf("reaching server: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusConflict:
		return fmt.Errorf("transition already in progress")
	case http.StatusNotFound:
		if isRouteNotFound(body) {
			return fmt.Errorf("server does not support %s — upgrade the homelab-manager binary on the server", url)
		}
		return fmt.Errorf("instance %d not found", vmid)
	default:
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// --- Styles ---

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("62")).
			Padding(0, 1)

	tierHeaderStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("12")).
			MarginTop(1)

	instanceStyle = lipgloss.NewStyle().
			PaddingLeft(2)

	greenDot  = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render("●")
	redDot    = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("●")
	yellowDot = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Render("●")

	doneStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("10")).
			MarginTop(1)

	errorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("9"))
)

func statusDot(status string) string {
	switch status {
	case "running":
		return greenDot
	case "stopped":
		return redDot
	default:
		return yellowDot
	}
}

func stateBadge(state string) string {
	var style lipgloss.Style
	switch state {
	case "awake":
		style = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	case "asleep":
		style = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	case "transitioning":
		style = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	default:
		style = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	}
	return style.Render(strings.ToUpper(state))
}

// --- runStatus: one-shot styled output ---

func runStatus(serverURL string) error {
	status, err := apiGetStatus(serverURL)
	if err != nil {
		return err
	}

	var b strings.Builder

	b.WriteString(titleStyle.Render("Homelab Status"))
	b.WriteString("  ")
	b.WriteString(stateBadge(status.State))
	b.WriteString("\n")

	for _, tier := range status.Tiers {
		header := fmt.Sprintf("Tier %d — %s", tier.Tier, tier.Name)
		b.WriteString(tierHeaderStyle.Render(header))
		b.WriteString("\n")

		for _, inst := range tier.Instances {
			line := fmt.Sprintf("%s  %-20s %s/%d",
				statusDot(inst.Status), inst.Name, inst.Type, inst.VMID)
			b.WriteString(instanceStyle.Render(line))
			b.WriteString("\n")
		}
	}

	fmt.Print(b.String())
	return nil
}

// --- transitionModel: Bubble Tea live TUI ---
//
// Used by runTier and runNight. The caller POSTs the action before
// launching the model; the model only polls /api/status until the
// transition flag clears.

type tickMsg time.Time
type statusMsg StatusResponse
type errMsg error

type transitionModel struct {
	serverURL string
	action    string // "wake" or "sleep"
	label     string // optional override for the title (e.g. "Tier 2")
	status    StatusResponse
	spinner   spinner.Model
	done      bool
	err       error
	started   bool
}

func newTransitionModel(serverURL, action string) transitionModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	return transitionModel{
		serverURL: serverURL,
		action:    action,
		spinner:   s,
	}
}

func (m transitionModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.fetchOnce)
}

func (m transitionModel) fetchOnce() tea.Msg {
	status, err := apiGetStatus(m.serverURL)
	if err != nil {
		return errMsg(err)
	}
	return statusMsg(status)
}

func fetchStatus(serverURL string) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(2 * time.Second)
		status, err := apiGetStatus(serverURL)
		if err != nil {
			return errMsg(err)
		}
		return statusMsg(status)
	}
}

func (m transitionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "q" || msg.String() == "ctrl+c" {
			return m, tea.Quit
		}

	case errMsg:
		m.err = msg
		return m, tea.Quit

	case statusMsg:
		m.status = StatusResponse(msg)
		m.started = true
		if !m.status.Transitioning {
			m.done = true
			return m, tea.Quit
		}
		return m, fetchStatus(m.serverURL)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m transitionModel) View() string {
	if m.err != nil {
		return errorStyle.Render(fmt.Sprintf("Error: %v", m.err)) + "\n"
	}

	var b strings.Builder

	actionLabel := "Waking"
	if m.action == "sleep" {
		actionLabel = "Sleeping"
	}
	target := "Homelab"
	if m.label != "" {
		target = m.label
	}

	if m.done {
		doneLabel := "awake"
		if m.action == "sleep" {
			doneLabel = "asleep"
		}
		b.WriteString(titleStyle.Render(fmt.Sprintf("%s %s", target, strings.ToUpper(doneLabel[:1])+doneLabel[1:])))
		b.WriteString("\n")
	} else {
		b.WriteString(titleStyle.Render(fmt.Sprintf("%s %s", actionLabel, target)))
		b.WriteString("  ")
		b.WriteString(m.spinner.View())
		b.WriteString("\n")
	}

	if m.started {
		for _, tier := range m.status.Tiers {
			header := fmt.Sprintf("Tier %d — %s", tier.Tier, tier.Name)
			b.WriteString(tierHeaderStyle.Render(header))
			b.WriteString("\n")

			for _, inst := range tier.Instances {
				line := fmt.Sprintf("%s  %-20s %s/%d",
					statusDot(inst.Status), inst.Name, inst.Type, inst.VMID)
				b.WriteString(instanceStyle.Render(line))
				b.WriteString("\n")
			}
		}
	}

	if m.done {
		doneLabel := "awake"
		if m.action == "sleep" {
			doneLabel = "asleep"
		}
		b.WriteString(doneStyle.Render(fmt.Sprintf("%s is %s.", target, doneLabel)))
		b.WriteString("\n")
	} else if !m.started {
		b.WriteString("\n  Starting...\n")
	} else {
		b.WriteString(lipgloss.NewStyle().Faint(true).MarginTop(1).Render("press q to detach"))
		b.WriteString("\n")
	}

	return b.String()
}

// runTier triggers a single-tier wake/sleep and waits (live TUI) for completion.
func runTier(serverURL, action string, tier int) error {
	if err := apiPostTier(serverURL, tier, action); err != nil {
		return err
	}

	m := newTransitionModel(serverURL, action)
	m.label = fmt.Sprintf("Tier %d", tier)
	p := tea.NewProgram(m)
	finalModel, err := p.Run()
	if err != nil {
		return err
	}
	if fm, ok := finalModel.(transitionModel); ok && fm.err != nil {
		return fm.err
	}
	return nil
}

// runNight triggers /api/night/{action} and waits (live TUI) for completion.
func runNight(serverURL, action string) error {
	if err := apiPostNight(serverURL, action); err != nil {
		return err
	}

	m := newTransitionModel(serverURL, action)
	m.label = "Night mode"
	p := tea.NewProgram(m)
	finalModel, err := p.Run()
	if err != nil {
		return err
	}
	if fm, ok := finalModel.(transitionModel); ok && fm.err != nil {
		return fm.err
	}
	return nil
}

// runInstance starts/stops a single instance synchronously and prints the result.
func runInstance(serverURL, action string, vmid int) error {
	if err := apiPostInstance(serverURL, vmid, action); err != nil {
		return err
	}

	verb := "started"
	if action == "stop" {
		verb = "stopped"
	}
	fmt.Printf("%s instance %d %s\n", greenDot, vmid, verb)
	return nil
}
