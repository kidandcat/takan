# Takan Design System

## Visual world

**Ink console** — a personal instrument panel. Cool paper white content, charcoal ink type, one jade accent for primary actions and live state. No parchment, no decorative glass.

## Color

| Token | Value | Role |
|-------|--------|------|
| `--bg` | `#f6f7f8` | App chrome |
| `--bg-soft` | `#eef0f2` | Sidebar / inset |
| `--bg-card` | `#ffffff` | Surfaces |
| `--text` | `#14171a` | Primary text |
| `--text-secondary` | `#5c6570` | Supporting |
| `--text-muted` | `#8b949e` | Meta |
| `--primary` | `#0f6e5a` | Actions, focus, links |
| `--primary-dark` | `#0b5445` | Hover primary |
| `--border` | `#e2e6ea` | Hairlines |
| `--ok` | `#0f6e5a` | Success / online |
| `--bad` | `#b42318` | Danger |
| `--warn` | `#a15c07` | Warning |

Light only (desk / phone daylight). Dark theme later if needed.

## Typography

- **UI:** `"DM Sans", system-ui, sans-serif` — one family for all product UI.
- **Mono:** `"IBM Plex Mono", ui-monospace, monospace` — codes, OTP, MCP URLs.
- Scale: 12 / 13 / 14 / 16 / 20 / 28 (rem). Heading weight 600–650; body 400–500.
- Tracking: headings −0.02em max.

## Layout

- Sidebar 248px sticky; content max readable width with generous padding.
- Spacing scale: 4 / 8 / 12 / 16 / 24 / 32 / 48.
- Cards: white, 1px border, soft shadow (offset + blur), radius 12px. No nested cards.
- Data tables for vault/lists; cards for modules and people.

## Components

- **Buttons:** solid primary (jade), secondary outline/neutral, ghost, danger.
- **Inputs:** white field, 1px border, 3px focus ring in primary tint.
- **Modals:** fixed overlay, centered dialog max 32rem, header + form + footer actions. Escape and backdrop close.
- **Pills:** status only (on / off / pending), not decoration.
- **Tables:** compact rows, hover row, actions trailing.

## Motion

150–200ms ease on hover/focus only. No page-load choreography.

## Vault-specific

- Secrets never shown in tables (except live OTP digits while session open).
- Add / edit always in modal; empty password/TOTP on edit = keep existing.
- Pending grants surface first with clear Approve / Deny.
