# Claude Code Multiplexer

### Reverse Claude Code

Claude distributes a huge binary by default in a Bun, which is difficult to reverse engineer (and is very likely the one the user have installed now).

You must obtain the npm version of the Claude Code js source code:

Example:

```bash
cd /tmp && npm pack @anthropic-ai/claude-code && tar xzf anthropic-ai-claude-code-*.tgz && npx prettier --write package/cli.js
```
