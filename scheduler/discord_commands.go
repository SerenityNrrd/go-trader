package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// readOnlyCommandNames are usable in a guild or in DMs by anyone.
var readOnlyCommandNames = map[string]bool{
	"status":           true,
	"health":           true,
	"positions":        true,
	"pnl":              true,
	"leaderboard":      true,
	"circuit-breakers": true,
	"dead-strategies":  true,
	"correlation":      true,
	"logs":             true,
}

// opsCommandNames mutate state or run heavy work; restricted to the owner in a DM.
var opsCommandNames = map[string]bool{
	"restart":  true,
	"backtest": true,
}

// authorizeCommand decides whether invokerID may run command `name`. Read-only
// commands are always allowed. Ops commands require the invoker to be the owner
// AND the interaction to be a DM (guildID == ""). Returns (false, reason) on deny.
func authorizeCommand(name, invokerID, guildID, ownerID string) (bool, string) {
	if readOnlyCommandNames[name] {
		return true, ""
	}
	if opsCommandNames[name] {
		if ownerID == "" {
			return false, "owner is not configured; ops commands are disabled"
		}
		if invokerID != ownerID {
			return false, "not authorized — this command is owner-only"
		}
		if guildID != "" {
			return false, "this command is only available in a DM with the bot"
		}
		return true, ""
	}
	return false, fmt.Sprintf("unknown command: %s", name)
}

// interactionUserID extracts the invoking user's ID from either a guild
// (i.Member.User) or DM (i.User) interaction.
func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// sortedAppStateIDs returns the strategy IDs of state in deterministic order.
func sortedAppStateIDs(state *AppState) []string {
	ids := make([]string, 0, len(state.Strategies))
	for id := range state.Strategies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// strategyPlatformLabel returns a human label for grouping (platform, else type).
func strategyPlatformLabel(s *StrategyState) string {
	if s.Platform != "" {
		return s.Platform
	}
	return s.Type
}

// positionMultiplier returns the PnL multiplier for a position (1 for spot).
func positionMultiplier(p *Position) float64 {
	if p.Multiplier > 0 {
		return p.Multiplier
	}
	return 1
}

// formatHealthResponse summarizes daemon liveness. `now` is injected for tests.
func formatHealthResponse(lastCycle time.Time, cycleCount int, version string, now time.Time) string {
	var sb strings.Builder
	sb.WriteString("**go-trader health**\n")
	sb.WriteString(fmt.Sprintf("version: %s\n", version))
	sb.WriteString(fmt.Sprintf("cycles completed: %d\n", cycleCount))
	if lastCycle.IsZero() {
		sb.WriteString("last cycle: never (no cycle completed yet)\n")
		sb.WriteString("status: starting")
		return sb.String()
	}
	age := now.Sub(lastCycle).Round(time.Second)
	status := "ok"
	if age > 30*time.Minute {
		status = "unhealthy (main loop stale)"
	}
	sb.WriteString(fmt.Sprintf("last cycle: %s ago\n", age))
	sb.WriteString(fmt.Sprintf("status: %s", status))
	return sb.String()
}

// formatStatusResponse builds a portfolio-wide one-line status. Call under RLock.
func formatStatusResponse(state *AppState, prices map[string]float64) string {
	var cash, value float64
	posCount, trades := 0, 0
	regime := ""
	for _, id := range sortedAppStateIDs(state) {
		s := state.Strategies[id]
		cash += s.Cash
		value += PortfolioValue(s, prices)
		posCount += len(s.Positions) + len(s.OptionPositions)
		trades += len(s.TradeHistory)
		if regime == "" && s.Regime != "" {
			regime = s.Regime
		}
	}
	return formatStatusLine(cash, posCount, value, trades, regime)
}
