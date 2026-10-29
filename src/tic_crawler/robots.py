"""robots.txt parsing and matching per RFC 9309.

Python's urllib.robotparser is not used: it applies the first matching rule instead of the
longest, ignores `*` and `$`, and in Python 3.12 a rule like `Disallow: /?` blocks every path.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from urllib.parse import unquote, urlsplit


@dataclass
class _Group:
    agents: list[str] = field(default_factory=list)
    rules: list[tuple[bool, str, re.Pattern]] = field(default_factory=list)  # (allow, path, regex)


def _normalize(path: str) -> str:
    # Compare decoded paths so %2F-style escapes and literal characters match the same way.
    return unquote(path)


def _compile(pattern: str) -> re.Pattern:
    anchored = pattern.endswith("$")
    body = pattern[:-1] if anchored else pattern
    rx = "".join(".*" if ch == "*" else re.escape(ch) for ch in _normalize(body))
    return re.compile(rx + ("$" if anchored else ""))


def product_token(user_agent: str) -> str:
    """'EverBetter-TiC-Crawler/1.0 (+x)' -> 'everbetter-tic-crawler'."""
    m = re.match(r"[A-Za-z_-]+", user_agent.strip())
    return (m.group(0) if m else user_agent).lower()


class RobotsRules:
    def __init__(self, groups: list[_Group] | None = None, allow_all: bool = False):
        self.groups = groups or []
        self.allow_all = allow_all

    @classmethod
    def parse(cls, text: str) -> RobotsRules:
        groups: list[_Group] = []
        current: _Group | None = None
        last_was_agent = False
        for raw in text.splitlines():
            line = raw.split("#", 1)[0].strip()
            if ":" not in line:
                continue
            key, _, value = line.partition(":")
            key, value = key.strip().lower(), value.strip()
            if key == "user-agent":
                if current is None or not last_was_agent:
                    current = _Group()
                    groups.append(current)
                current.agents.append(value.lower())
                last_was_agent = True
            elif key in ("allow", "disallow"):
                last_was_agent = False
                if current is None or not value:
                    continue  # an empty Disallow allows everything; rules outside a group are ignored
                current.rules.append((key == "allow", value, _compile(value)))
            else:
                last_was_agent = False
        return cls(groups)

    def _group_for(self, user_agent: str) -> list[tuple[bool, str, re.Pattern]] | None:
        token = product_token(user_agent)
        matched = [g for g in self.groups if token in g.agents]
        if not matched:
            matched = [g for g in self.groups if "*" in g.agents]
        if not matched:
            return None
        rules: list = []
        for g in matched:  # RFC 9309: groups for the same agent are combined
            rules.extend(g.rules)
        return rules

    def can_fetch(self, user_agent: str, url: str) -> bool:
        if self.allow_all:
            return True
        parts = urlsplit(url)
        path = _normalize((parts.path or "/") + (f"?{parts.query}" if parts.query else ""))
        if path == "/robots.txt":
            return True
        rules = self._group_for(user_agent)
        if not rules:
            return True
        best: tuple[int, bool] | None = None
        for allow, pattern, rx in rules:
            if rx.match(path):
                length = len(pattern)
                if best is None or length > best[0] or (length == best[0] and allow):
                    best = (length, allow)
        return True if best is None else best[1]
