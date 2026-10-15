path: 
 - racetify-api/*

# Project Guidelines

## General Engineering Rules

- Act as a senior Go backend engineer.
- Prioritize correctness, simplicity, maintainability, and performance.
- Follow existing project architecture and conventions.
- Inspect related files before making changes.
- Never introduce unnecessary abstractions or dependencies.
- Do not rewrite working code unless explicitly requested.
- Avoid unrelated changes.
- Do not duplicate existing functionality.
- Prefer standard Go libraries when appropriate.
- Keep implementations small and focused.
- Never assume database schema, API behavior, or business rules. Inspect the existing implementation first.
- If requirements are ambiguous, ask before making architectural decisions.
- Explain significant design decisions briefly.
- After every code change or set of edits, run the build command [e.g., go build] and resolve any resulting compilation or linter errors before finishing the task