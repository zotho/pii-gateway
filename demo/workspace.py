"""Sandboxed workspace + tools (ls/read/grep/write) for the demo agent.

The workspace is seeded from fixtures/canary-repo/ (fake PII only)."""
from __future__ import annotations

import re
import shutil
from pathlib import Path

MAX_READ = 20000
MAX_WRITE = 100000

FIXTURE = Path(__file__).resolve().parents[1] / "fixtures" / "canary-repo"


class SandboxError(Exception):
    pass


class Workspace:
    def __init__(self, root: Path) -> None:
        self.root = Path(root).resolve()

    def seed(self) -> None:
        if self.root.exists():
            shutil.rmtree(self.root)
        shutil.copytree(FIXTURE, self.root)

    def resolve(self, path: str) -> Path:
        """Resolve `path` inside the workspace; symlinks are followed, so escapes are caught too."""
        if not isinstance(path, str) or "\0" in path:
            raise SandboxError("invalid path")
        p = Path(path)
        if p.is_absolute():
            raise SandboxError("absolute paths are not allowed")
        full = (self.root / p).resolve()
        if full != self.root and self.root not in full.parents:
            raise SandboxError("path escapes workspace")
        return full

    # ---- tools: always return str, never raise
    def ls(self, path: str = ".") -> str:
        try:
            p = self.resolve(path or ".")
            if not p.is_dir():
                return f"error: not a directory: {path}"
            return "\n".join(sorted(c.name + ("/" if c.is_dir() else "") for c in p.iterdir())) or "(empty)"
        except SandboxError as e:
            return f"error: {e}"

    def read(self, path: str) -> str:
        try:
            p = self.resolve(path)
            if not p.is_file():
                return f"error: no such file: {path}"
            return p.read_text(encoding="utf-8", errors="replace")[:MAX_READ]
        except SandboxError as e:
            return f"error: {e}"

    def grep(self, pattern: str, path: str = ".") -> str:
        try:
            base = self.resolve(path or ".")
            rx = re.compile(pattern)
        except SandboxError as e:
            return f"error: {e}"
        except re.error as e:
            return f"error: bad pattern: {e}"
        files = [base] if base.is_file() else sorted(f for f in base.rglob("*") if f.is_file())
        out = []
        for f in files:
            try:
                f.resolve().relative_to(self.root)
                for i, line in enumerate(f.read_text(encoding="utf-8", errors="replace").splitlines(), 1):
                    if rx.search(line):
                        out.append(f"{f.relative_to(self.root)}:{i}:{line}")
            except (ValueError, OSError):
                continue
        return "\n".join(out[:200]) or "(no matches)"

    def write(self, path: str, content: str) -> str:
        try:
            p = self.resolve(path)
            if len(content) > MAX_WRITE:
                return "error: content too large"
            if p == self.root or p.is_dir():
                return f"error: is a directory: {path}"
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text(content, encoding="utf-8")
            return f"wrote {len(content)} chars to {path}"
        except SandboxError as e:
            return f"error: {e}"

    def call(self, name: str, args: dict) -> str:
        fn = {"ls": self.ls, "read": self.read, "grep": self.grep, "write": self.write}.get(name)
        if fn is None:
            return f"error: unknown tool {name}"
        try:
            return fn(**(args or {}))
        except TypeError as e:
            return f"error: bad arguments: {e}"


TOOLS = [
    {"type": "function", "name": "ls", "description": "List a directory in the workspace.",
     "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}},
    {"type": "function", "name": "read", "description": "Read a text file.",
     "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}},
    {"type": "function", "name": "grep", "description": "Regex search in files under path.",
     "parameters": {"type": "object", "properties": {"pattern": {"type": "string"}, "path": {"type": "string"}},
                    "required": ["pattern", "path"]}},
    {"type": "function", "name": "write", "description": "Write a text file.",
     "parameters": {"type": "object", "properties": {"path": {"type": "string"}, "content": {"type": "string"}},
                    "required": ["path", "content"]}},
]
