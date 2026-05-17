package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type usageModel struct {
	client   *Client
	viewport viewport.Model
	content  string
	err      error
	width    int
	height   int
	ready    bool

	lastSummary map[string]any
}

type usageDataMsg struct {
	summary map[string]any
	err     error
}

func newUsageModel(client *Client) usageModel {
	return usageModel{client: client}
}

func (m usageModel) Init() tea.Cmd {
	return m.fetchData
}

func (m usageModel) fetchData() tea.Msg {
	summary, err := m.client.GetUsageSummary("7d")
	return usageDataMsg{summary: summary, err: err}
}

func (m usageModel) Update(msg tea.Msg) (usageModel, tea.Cmd) {
	switch msg := msg.(type) {
	case localeChangedMsg:
		m.content = m.renderUsage(m.lastSummary)
		m.viewport.SetContent(m.content)
		return m, m.fetchData
	case usageDataMsg:
		if msg.err != nil {
			m.err = msg.err
			m.content = errorStyle.Render(T("error_prefix") + msg.err.Error())
		} else {
			m.err = nil
			m.lastSummary = msg.summary
			m.content = m.renderUsage(msg.summary)
		}
		m.viewport.SetContent(m.content)
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "r" {
			return m, m.fetchData
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m *usageModel) SetSize(w, h int) {
	m.width = w
	m.height = h
	if !m.ready {
		m.viewport = viewport.New(w, h)
		m.viewport.SetContent(m.content)
		m.ready = true
	} else {
		m.viewport.Width = w
		m.viewport.Height = h
	}
}

func (m usageModel) View() string {
	if !m.ready {
		return T("loading")
	}
	return m.viewport.View()
}

func (m usageModel) renderUsage(summary map[string]any) string {
	var sb strings.Builder

	sb.WriteString(titleStyle.Render(T("usage_title")))
	sb.WriteString("\n")
	sb.WriteString(helpStyle.Render(T("usage_help")))
	sb.WriteString("\n\n")

	if summary == nil {
		sb.WriteString(T("usage_no_data"))
		sb.WriteString("\n")
		return sb.String()
	}

	currency := getString(summary, "currency")
	if currency == "" {
		currency = "USD"
	}
	total := getMapField(summary, "total")
	tokens := getMapField(total, "tokens")
	requests := int64(getNumber(total, "requests"))
	success := int64(getNumber(total, "success"))
	failed := int64(getNumber(total, "failed"))
	totalTokens := int64(getNumber(tokens, "total_tokens"))
	inputTokens := int64(getNumber(tokens, "input_tokens"))
	outputTokens := int64(getNumber(tokens, "output_tokens"))
	cachedTokens := int64(getNumber(tokens, "cached_tokens"))
	reasoningTokens := int64(getNumber(tokens, "reasoning_tokens"))
	cost := getNumber(total, "estimated_cost")
	priced := getBool(total, "priced")

	cardWidth := 25
	if m.width > 0 {
		cardWidth = (m.width - 2) / 2
		if cardWidth < 18 {
			cardWidth = 18
		}
	}
	cardStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Padding(0, 1).
		Width(cardWidth).
		Height(2)

	card1 := cardStyle.Render(fmt.Sprintf("%s\n%s",
		valueStyle.Render(formatLargeNumber(requests)),
		labelStyle.Render(T("usage_total_reqs"))))
	card2 := cardStyle.Render(fmt.Sprintf("%s\n%s",
		valueStyle.Render(formatLargeNumber(totalTokens)),
		labelStyle.Render(T("usage_total_tokens"))))
	sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, card1, " ", card2))
	sb.WriteString("\n")

	card3 := cardStyle.Render(fmt.Sprintf("%s\n%s",
		valueStyle.Render(fmt.Sprintf("%d / %d", success, failed)),
		labelStyle.Render(T("usage_success")+" / "+T("usage_failure"))))
	card4 := cardStyle.Render(fmt.Sprintf("%s\n%s",
		valueStyle.Render(formatUsageCost(currency, cost, priced)),
		labelStyle.Render(T("usage_estimated_cost"))))
	sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, card3, " ", card4))
	sb.WriteString("\n\n")

	sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(colorHighlight).Render(T("usage_token_detail")))
	sb.WriteString("\n")
	sb.WriteString(formatKV(T("usage_input"), formatLargeNumber(inputTokens)))
	sb.WriteString(formatKV(T("usage_output"), formatLargeNumber(outputTokens)))
	sb.WriteString(formatKV(T("usage_cached"), formatLargeNumber(cachedTokens)))
	sb.WriteString(formatKV(T("usage_reasoning"), formatLargeNumber(reasoningTokens)))
	sb.WriteString("\n")

	sb.WriteString(lipgloss.NewStyle().Bold(true).Foreground(colorHighlight).Render(T("model_stats")))
	sb.WriteString("\n")
	sb.WriteString(strings.Repeat("─", minInt(m.width, 90)))
	sb.WriteString("\n")
	byModel := getMapSliceField(summary, "by_model")
	if len(byModel) == 0 {
		sb.WriteString(T("usage_no_data"))
		sb.WriteString("\n")
		return sb.String()
	}
	for i, row := range byModel {
		if i >= 15 {
			break
		}
		rowTokens := getMapField(row, "tokens")
		provider := getString(row, "provider")
		model := getString(row, "model")
		name := strings.Trim(provider+"/"+model, "/")
		if name == "" {
			name = getString(row, "key")
		}
		line := fmt.Sprintf("  %-34s %8s req  %10s tok  %s\n",
			truncate(name, 34),
			formatLargeNumber(int64(getNumber(row, "requests"))),
			formatLargeNumber(int64(getNumber(rowTokens, "total_tokens"))),
			formatUsageCost(currency, getNumber(row, "estimated_cost"), getBool(row, "priced")),
		)
		sb.WriteString(line)
	}
	return sb.String()
}

func formatUsageCost(currency string, cost float64, priced bool) string {
	if !priced {
		return T("usage_not_priced")
	}
	return fmt.Sprintf("%s %.4f", currency, cost)
}

func getMapField(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil
	}
	if value, ok := raw.(map[string]any); ok {
		return value
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var value map[string]any
	if err = json.Unmarshal(data, &value); err != nil {
		return nil
	}
	return value
}

func getMapSliceField(m map[string]any, key string) []map[string]any {
	if m == nil {
		return nil
	}
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var value []map[string]any
	if err = json.Unmarshal(data, &value); err != nil {
		return nil
	}
	return value
}

func getNumber(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	switch value := m[key].(type) {
	case float64:
		return value
	case float32:
		return float64(value)
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case json.Number:
		f, _ := value.Float64()
		return f
	default:
		return 0
	}
}
