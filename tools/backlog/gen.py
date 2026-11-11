#!/usr/bin/env python3
"""Emit docs/ISSUES.md and the GitHub label/milestone/issue payloads."""
import json, os, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import backlog, part2

ISSUES = backlog.ISSUES + part2.MORE
ROOT = "/home/kimongi/code/myjobfyt"

MILESTONE_ORDER = [
    ("M1 — CV Intelligence", "Turn an uploaded document into structured, matchable candidate data, and generate tailored materials that never fabricate anything."),
    ("M2 — Job Acquisition", "Bring real postings in from lawfully accessible sources, normalised and deduplicated, with loops as the search abstraction."),
    ("M3 — Matching", "Score postings against candidates explainably, and retain rejections with reasons so a thin funnel is self-diagnosable."),
    ("M4 — Application Orchestration", "Prepare, authorise and submit applications, with consent recorded and duplicates structurally impossible."),
    ("M5 — Frontend", "A polished interface where every visible feature is wired to real backend behaviour and no number is presented more confidently than the data supports."),
    ("M6 — Outreach, Analytics & Admin", "Lawful recruiter outreach, analytics that reconcile with the event log, operational visibility, billing and GDPR obligations."),
    ("M7 — Infrastructure & GitOps", "Containerise, chart, and deploy to a VPS Kubernetes cluster reconciled from Git, with observability and a tested restore path."),
    ("M8 — CI/CD & Hardening", "Separate CI from CD, make the safety invariants continuously verified rather than verified once, and document the security posture."),
]

PRIORITY_COLOURS = {"critical": "b60205", "high": "d93f0b", "medium": "fbca04", "low": "0e8a16"}
AREA_COLOURS = {
    "architecture": "5319e7", "backend": "1d76db", "frontend": "0052cc", "database": "006b75",
    "ai": "8b5cf6", "job-acquisition": "0b7285", "integrations": "0969da", "security": "b60205",
    "infrastructure": "5a5a5a", "devops": "444444", "observability": "c2185b",
    "testing": "0e8a16", "performance": "e99695", "documentation": "bfdadc",
}
COMPLEXITY_COLOURS = {"XS": "ededed", "S": "d4d4d4", "M": "b0b0b0", "L": "8a8a8a", "XL": "5f5f5f"}


def issue_body(i):
    deps = ", ".join(i["deps"]) if i["deps"] else "none"
    acceptance = "\n".join(f"- [ ] {a}" for a in i["acceptance"])
    return f"""**{i['id']}**

## Problem

{i['problem']}

## Business impact

{i['business']}

## Technical impact

{i['technical']}

## Proposed solution

{i['solution']}

## Acceptance criteria

{acceptance}

## Testing requirements

{i['testing']}

---

| | |
|---|---|
| **Priority** | {i['priority']} |
| **Complexity** | {i['complexity']} |
| **Subsystem** | {i['area']} |
| **Milestone** | {i['milestone']} |
| **Depends on** | {deps} |
"""


def write_issues_md():
    by_ms = {}
    for i in ISSUES:
        by_ms.setdefault(i["milestone"], []).append(i)

    blocked_by = {}
    for i in ISSUES:
        for d in i["deps"]:
            blocked_by.setdefault(d, []).append(i["id"])

    out = ["""# Engineering Backlog

61 issues across 8 milestones, each specified against the template in the engineering
directive: problem, business impact, technical impact, proposed solution, acceptance
criteria, dependencies, priority, complexity, subsystem and testing requirements.

This file and the GitHub issues are generated from one definition in
`tools/backlog/`. A backlog that exists in two places is a backlog nobody trusts, so
edit the definition and regenerate rather than editing either output.

## Summary
"""]
    from collections import Counter
    pc = Counter(i["priority"] for i in ISSUES)
    cc = Counter(i["complexity"] for i in ISSUES)
    out.append("| Priority | Count |\n|---|---|")
    for p in ["critical", "high", "medium", "low"]:
        out.append(f"| {p} | {pc.get(p,0)} |")
    out.append("\n| Complexity | Count |\n|---|---|")
    for c in ["S", "M", "L", "XL"]:
        out.append(f"| {c} | {cc.get(c,0)} |")
    out.append("")

    out.append("| Milestone | Issues | Critical |\n|---|---|---|")
    for name, _ in MILESTONE_ORDER:
        items = by_ms.get(name, [])
        crit = sum(1 for i in items if i["priority"] == "critical")
        out.append(f"| {name} | {len(items)} | {crit} |")
    out.append("")

    # The few issues that gate the most other work are worth calling out explicitly:
    # they are where a delay costs the most.
    gates = sorted(blocked_by.items(), key=lambda kv: -len(kv[1]))[:6]
    out.append("## Critical path\n")
    out.append("The issues that block the most other work. A delay here is a delay everywhere.\n")
    out.append("| Issue | Blocks |\n|---|---|")
    titles = {i["id"]: i["title"] for i in ISSUES}
    for iid, blocks in gates:
        out.append(f"| `{iid}` {titles[iid]} | {', '.join(blocks)} |")
    out.append("")

    for name, description in MILESTONE_ORDER:
        out.append(f"\n---\n\n# {name}\n\n{description}\n")
        for i in by_ms.get(name, []):
            deps = ", ".join(f"`{d}`" for d in i["deps"]) if i["deps"] else "none"
            blocks = ", ".join(f"`{b}`" for b in blocked_by.get(i["id"], [])) or "none"
            acceptance = "\n".join(f"- [ ] {a}" for a in i["acceptance"])
            out.append(f"""## {i['id']} — {i['title']}

`{i['priority']}` · `{i['area']}` · complexity `{i['complexity']}`

**Problem.** {i['problem']}

**Business impact.** {i['business']}

**Technical impact.** {i['technical']}

**Proposed solution.** {i['solution']}

**Acceptance criteria.**

{acceptance}

**Testing.** {i['testing']}

**Depends on** {deps}. **Blocks** {blocks}.
""")

    with open(f"{ROOT}/docs/ISSUES.md", "w") as f:
        f.write("\n".join(out))
    return len(ISSUES)


def write_payloads():
    labels = []
    for p, c in PRIORITY_COLOURS.items():
        labels.append({"name": f"priority/{p}", "color": c, "description": f"{p} priority"})
    for a, c in AREA_COLOURS.items():
        labels.append({"name": f"area/{a}", "color": c, "description": f"{a} subsystem"})
    for k, c in COMPLEXITY_COLOURS.items():
        labels.append({"name": f"complexity/{k}", "color": c, "description": f"complexity {k}"})

    milestones = [{"title": n, "description": d} for n, d in MILESTONE_ORDER]

    issues = [{
        "id": i["id"],
        "title": f"{i['id']} — {i['title']}",
        "body": issue_body(i),
        "labels": [f"priority/{i['priority']}", f"area/{i['area']}", f"complexity/{i['complexity']}"],
        "milestone": i["milestone"],
    } for i in ISSUES]

    here = os.path.dirname(os.path.abspath(__file__))
    for name, data in [("labels.json", labels), ("milestones.json", milestones), ("issues.json", issues)]:
        with open(os.path.join(here, name), "w") as f:
            json.dump(data, f, indent=1)
    return len(labels), len(milestones), len(issues)


if __name__ == "__main__":
    n = write_issues_md()
    l, m, i = write_payloads()
    print(f"docs/ISSUES.md: {n} issues")
    print(f"payloads: {l} labels, {m} milestones, {i} issues")
