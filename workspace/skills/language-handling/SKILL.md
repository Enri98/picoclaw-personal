---
name: language-handling
description: Rules for choosing Italian or English in replies and wiki writes.
---

# Language handling

## Reply language

Respond in the language of the user's most recent message. If the language is
ambiguous, default to English. Never volunteer a translation.

## Wiki writes

The wiki is primarily English. When proposing a wiki write:

- Write the body in English unless the source content is Italian **and**
  translating it would lose meaningful nuance.
- In that case, keep the body in Italian and add an English `summary` field in
  the frontmatter.
- Frontmatter keys and values are always English regardless of body language.

## Voice notes

Voice transcripts include both `transcript_original` (source language) and
`transcript_english`. Use whichever is appropriate for downstream processing;
the reply to the user uses the detected language.
