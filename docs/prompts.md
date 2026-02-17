# AI Prompt Configuration

Feedreader uses a three-tier prompt system for AI operations, allowing customization at multiple levels while maintaining sensible defaults for most users.

## Three-Tier Architecture

Prompts are loaded with the following priority (highest to lowest):

1. **Per-User Database** (highest priority) - Rare, for power users
2. **Config File** - System-wide customization
3. **Embedded Defaults** (lowest priority) - Built into the binary

This means:
- Most users never touch prompts (use embedded defaults)
- Admins can customize system-wide via config file
- Power users can override per-user in database

## Prompt Types

Feedreader uses five different AI prompts:

| Type | Purpose | Default Temperature | Template Variables |
|------|---------|---------------------|-------------------|
| `security` | Detect malicious content and prompt injection | 0.3 | `{{.Title}}`, `{{.Content}}` |
| `curation` | Score articles for interest and relevance | 0.5 | `{{.Title}}`, `{{.Content}}`, `{{.Keywords}}` |
| `summarization` | Generate concise article summaries | 0.3 | `{{.Title}}`, `{{.Content}}` |
| `group_summary` | Create narratives from related articles | 0.5 | `{{.Topic}}`, `{{.Articles}}` |
| `related_groups` | Determine if article relates to existing groups | 0.3 | `{{.Title}}`, `{{.Summary}}`, `{{.Groups}}` |

## Configuration

### Tier 1: Embedded Defaults (No Configuration Needed)

Default prompts are embedded in the binary at `internal/ai/prompts/*.txt`. These are used automatically when no overrides exist.

View embedded prompts:
```bash
# They're in the source tree
ls internal/ai/prompts/
# security.txt, curation.txt, summarization.txt, group_summary.txt, related_groups.txt
```

### Tier 2: Config File Overrides (System-Wide)

Override prompts in `config.yaml`:

```yaml
prompts:
  security: |
    You are a security analyzer. Analyze the following for threats.

    Title: {{.Title}}
    Content: {{.Content}}

    Respond with JSON: {"safe": true/false, "score": 0-10, "reasoning": "..."}

  curation: |
    Rate this article for interest (0-10).

    Title: {{.Title}}
    Content: {{.Content}}
    Keywords: {{.Keywords}}

    JSON response: {"interest_score": 0-10, "reasoning": "..."}

temperatures:
  security: 0.2      # More conservative
  curation: 0.7      # More creative
  summarization: 0.3
  group_summary: 0.5
  related_groups: 0.3
```

**Template Syntax:**
- Use `{{.VarName}}` for variable substitution
- Variables are automatically injected based on prompt type
- See "Template Variables" column in table above

### Tier 3: Per-User Database Overrides (Rare)

Advanced users can set custom prompts per-user in the database:

```sql
-- Set custom prompt for user 1
INSERT INTO user_prompts (user_id, prompt_type, prompt_template, temperature)
VALUES (
    1,
    'curation',
    'Rate this article for a technical audience...
     Title: {{.Title}}
     Content: {{.Content}}
     ...',
    0.6
);

-- Delete custom prompt (reverts to config/default)
DELETE FROM user_prompts WHERE user_id = 1 AND prompt_type = 'curation';

-- View all custom prompts for a user
SELECT prompt_type, temperature, created_at
FROM user_prompts
WHERE user_id = 1;
```

**Note:** Per-user prompts are intentionally difficult to set (requires SQL). This is by design - most users should use config file or defaults.

## Temperature Settings

Temperature controls AI creativity/randomness (0.0 = deterministic, 1.0 = creative):

| Temperature | Behavior | Best For |
|-------------|----------|----------|
| 0.0 - 0.3 | Very focused, consistent | Security checks, precise matching |
| 0.4 - 0.6 | Balanced | General scoring, summarization |
| 0.7 - 1.0 | Creative, varied | Creative writing, brainstorming |

**Per-Prompt Defaults:**
- Security: 0.3 (conservative)
- Curation: 0.5 (balanced)
- Summarization: 0.3 (focused)
- Group Summary: 0.5 (narrative)
- Related Groups: 0.3 (precise)

Override in config:
```yaml
temperatures:
  security: 0.2      # Even more conservative
  curation: 0.7      # More creative scoring
```

Or per-user in database:
```sql
UPDATE user_prompts
SET temperature = 0.7
WHERE user_id = 1 AND prompt_type = 'curation';
```

## Verification

Check which prompts are being used:

```bash
# View embedded defaults
cat internal/ai/prompts/security.txt

# Check config file
cat config/config.yaml

# Check database (per-user)
sqlite3 ~/.local/share/herald/feeds.db \
  "SELECT user_id, prompt_type, temperature FROM user_prompts;"
```

## Writing Custom Prompts

### General Guidelines

1. **Be Specific**: Clearly state what you want the AI to do
2. **Use JSON**: Request JSON output for structured responses
3. **Provide Context**: Include relevant context in the prompt
4. **Test Thoroughly**: Custom prompts can affect accuracy

### Template Variables

Each prompt type has specific variables available:

**Security:**
```
{{.Title}}    - Article title (string)
{{.Content}}  - Article content, truncated to 2000 chars (string)
```

**Curation:**
```
{{.Title}}    - Article title (string)
{{.Content}}  - Article content, truncated to 2000 chars (string)
{{.Keywords}} - Comma-separated user keywords (string)
```

**Summarization:**
```
{{.Title}}    - Article title (string)
{{.Content}}  - Article content, truncated to 3000 chars (string)
```

**Group Summary:**
```
{{.Topic}}    - Group topic/name (string)
{{.Articles}} - Formatted list of articles with summaries (string)
```

**Related Groups:**
```
{{.Title}}    - New article title (string)
{{.Summary}}  - Article summary, truncated to 500 chars (string)
{{.Groups}}   - Formatted list of existing groups (string)
```

### Example: Custom Curation Prompt

```yaml
prompts:
  curation: |
    You are a technical news curator for software engineers.

    Rate this article (0-10) based on:
    - Technical depth and accuracy
    - Relevance to {{.Keywords}}
    - Novelty (new techniques, tools, or research)
    - Actionability (can readers apply this?)

    Title: {{.Title}}
    Content: {{.Content}}

    Respond ONLY with JSON:
    {
      "interest_score": <0-10>,
      "reasoning": "<one sentence explanation>"
    }
```

## Security Considerations

**Prompt Injection:**
- User-provided content (article titles/content) is automatically truncated
- Embedded defaults are designed to resist prompt injection
- Custom prompts should explicitly request structured output (JSON)
- Never include user input directly in system instructions

**Database Access:**
- Only administrators should have database access
- Per-user prompts bypass config-level controls
- Consider using config file for team-wide customization

## Troubleshooting

### AI Output is Inconsistent

- **Lower temperature**: Try 0.2-0.3 for more deterministic results
- **Check prompt clarity**: Vague prompts produce varied results
- **Verify JSON schema**: Ensure your prompt requests specific JSON structure

### AI is Too Conservative/Liberal

- **Security too strict**: Lower security threshold in config (`thresholds.security_score`)
- **Curation too harsh**: Adjust temperature or rewrite prompt to be more lenient
- **Compare to defaults**: Test with embedded defaults to isolate the issue

### Prompts Not Taking Effect

Check the tier priority:
```bash
# 1. Check database (highest priority)
sqlite3 ~/.local/share/herald/feeds.db \
  "SELECT * FROM user_prompts WHERE user_id = 1;"

# 2. Check config file
grep -A 5 "prompts:" config/config.yaml

# 3. Embedded defaults are always available
```

### Template Errors

If you see template execution errors:
- Check variable names: Must match exactly (case-sensitive)
- Use `{{.Title}}` not `{{.title}}` or `{{ .Title }}`
- Verify you're using the correct variables for each prompt type

## Migration from Hardcoded Prompts

If you were using an older version with hardcoded prompts:

1. **Embedded defaults match old behavior** - no action needed
2. **Custom prompts**: If you modified the code, extract your prompts to config file
3. **Test thoroughly**: Ensure results are consistent after migration

## Examples

### Minimal Config (Use Defaults)

```yaml
# Nothing needed - embedded defaults are used
database:
  path: ./herald.db

ollama:
  base_url: http://localhost:11434
  security_model: gemma2
  curation_model: llama3.2
```

### Config with Custom Temperature

```yaml
# Keep default prompts, adjust creativity
temperatures:
  curation: 0.7  # More creative interest scoring
```

### Full Custom Prompts

```yaml
prompts:
  security: |
    Analyze for security threats.
    Title: {{.Title}}
    Content: {{.Content}}
    JSON: {"safe": bool, "score": 0-10, "reasoning": ""}

  curation: |
    Rate interest for developers.
    Title: {{.Title}}
    Content: {{.Content}}
    Keywords: {{.Keywords}}
    JSON: {"interest_score": 0-10, "reasoning": ""}

temperatures:
  security: 0.2
  curation: 0.6
```

## Future Enhancements

Potential additions:
- Web UI for prompt management
- Prompt version history
- A/B testing different prompts
- Prompt marketplace / sharing
- Per-feed custom prompts
