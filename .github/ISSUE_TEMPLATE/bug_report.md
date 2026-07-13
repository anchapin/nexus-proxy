name: bug_report
about: Report a bug in Nexus Proxy
title: '[BUG] '
labels: ['bug']
body:
  - type: markdown
    attributes:
      value: |
        Thanks for reporting a bug! Please provide as much detail as possible to help us fix it.
  - type: input
    id: version
    attributes:
      label: Nexus Proxy Version
      description: The version or commit hash you are using.
    validations:
      required: true
  - type: input
    id: environment
    attributes:
      label: Environment Details
      description: OS, Go version, Ollama version, etc.
    validations:
      required: true
  - type: textarea
    id: repro
    attributes:
      label: Steps to Reproduce
      description: A clear and concise description of what led to the bug.
    validations:
      required: true
  - type: textarea
    id: expected
    attributes:
      label: Expected Behavior
    validations:
      required: true
  - type: textarea
    id: actual
    attributes:
      label: Actual Behavior
    validations:
      required: true
  - type: textarea
    id: logs
    attributes:
      label: Logs
      description: Please paste relevant logs or error messages.
