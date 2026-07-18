# Contributing to Nexus Proxy

Thank you for your interest in contributing to Nexus Proxy! We welcome contributions that improve the routing efficiency, reliability, and developer experience of the gateway.

## How to Contribute

### Filing Issues and Feature Requests
We use GitHub Issues to track bugs and requested features. Please use the provided issue templates in `.github/ISSUE_TEMPLATE/` to ensure we have all the necessary information to diagnose and implement the change.

- **Bug Reports**: Include a minimal reproduction, your environment details, and the expected vs. actual behavior.
- **Feature Requests**: Describe the problem you're solving, what success looks like, and any alternatives you've considered.

### Development Workflow
1. **Branching**: Create a feature or fix branch from `develop` (e.g., `fix/issue-123` or `feat/new-router-rule`). The `develop` branch is the default base for all PRs. The `main` branch is only used as the base for PRs from `develop` when preparing a new release.
2. **Commits**: Use [Conventional Commits](https://www.conventionalcommits.org/) (e.g., `feat: ...`, `fix: ...`, `docs: ...`).
3. **Testing**: Every change must be accompanied by tests. Use `make test` to run the suite.
4. **CI**: Ensure `make ci` passes locally before submitting a Pull Request.

### Project Conventions
- **Middleware Order**: Do not reorder middleware in `cmd/nexus/main.go` casually. The order of security headers, rate limiting, and authentication is critical for security.
- **Environment Variables**: There is no central registry to edit. When adding a new configuration option: (1) add the field to the `Config` struct in `internal/config/config.go` and parse it inline in `Load()` using a `getEnv*` helper (`getEnv`, `getEnvAllowEmpty`, `getEnvInt`, `getEnvBool`, `getEnvFloat`, `getEnvDuration`, `getEnvRegexps`); (2) mirror the field on `YAMLConfig` and add the env override branch in `LoadYAML()` (`internal/config/yaml.go`) so config-file users get the same knob; (3) document the variable in `.env.example`.
- **Logging**: Use `log/slog` for structured logging. Avoid `fmt.Println` in production paths.
- **Dependency Rule**: Follow the architecture outlined in `Nexus Proxy PRD and Architecture.md`. Avoid circular dependencies between `internal/` packages.

### Local Setup
- **Prerequisites**: Install Go 1.21+ and ensure Ollama is running locally.
- **Build**: Run `make build`.
- **Verify**: Run `./bin/nexus check` (alias `./bin/nexus doctor`) to
  validate boot-time configuration before serving traffic. Exits `0`
  on pass (warnings + skips are fine), `1` when at least one check
  fails — see the Quickstart in `README.md` for the full table.
- **Run**: `./bin/nexus`.
- **Test**: `make test`.
- **Lint**: `make lint`.

## Guidelines
- Be respectful and inclusive in all communications.
- Follow the existing Go style and project patterns.
- Keep changes atomic and focused.
