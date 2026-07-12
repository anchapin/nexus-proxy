name: feature_request
about: Propose a new feature for Nexus Proxy
title: '[FEATURE] '
labels: ['enhancement']
body:
  - type: markdown
    attributes:
      value: |
        Thanks for suggesting a feature! We love hearing from our community.
  - type: input
    id: goal
    attributes:
      label: Goal
      description: What problem does this feature solve?
    validations:
      required: true
  - type: textarea
    id: description
    attributes:
      label: Proposed Implementation
      description: How do you envision this working?
    validations:
      required: true
  - type: textarea
    id: alternatives
    attributes:
      label: Alternatives Considered
      description: Have you thought of other ways to solve this?
  - type: checkbox
    id: value_proposition
    attributes:
      label: Does this align with the core mission of being a lightweight, high-performance gateway?
