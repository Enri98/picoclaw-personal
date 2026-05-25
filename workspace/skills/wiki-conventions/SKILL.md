# Wiki conventions

## Frontmatter

Every wiki page starts with a YAML frontmatter block:

```
---
title: Short descriptive title
type: person | project | topic | journal | reference
tags: [tag1, tag2]
aliases: []
updated: YYYY-MM-DD
---
```

Required: `title`, `type`, `updated`.  
Optional: `tags`, `aliases`.

## File naming

- Lowercase, hyphens for spaces: `my-project.md`
- No special characters except `-` and `_`
- Organised under top-level directories: `people/`, `projects/`, `work/`, `topics/`, `journal/`, `reminders.md`, `inbox.md`
- Journal entries: `journal/YYYY-MM-DD.md`

## Body language

- Plain markdown; use `##` for sections, `-` for lists
- Cross-link with `[[page-name]]` (Obsidian-style)
- Keep entries factual and terse; avoid filler prose
- Dates in ISO 8601: `2026-05-25`

## Inbox workflow

`inbox.md` is a capture buffer. Items land there via `wiki_append_to_inbox` or `/note`.  
Periodic review promotes items to permanent pages or discards them.  
Do not write prose directly to inbox — bullet points only.

## Writing pages

- **Quick capture** (voice, Telegram quick note): use `wiki_append_to_inbox`.  
  This writes directly and commits; no approval needed.
- **New or updated page**: use `wiki_propose_write`.  
  This creates a pending proposal. Enrico runs `/apply <id>` to confirm.  
  Proposals expire in 15 minutes.

## Reading and searching

- `wiki_search` for keyword lookup — checks title, tags, then body.  
  Returns up to 10 results; frontmatter matches rank higher.
- `wiki_read` for full page content (path relative to wiki root).
- `wiki_list` to browse a directory (`""` for root).
