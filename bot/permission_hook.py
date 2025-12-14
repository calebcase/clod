#!/usr/bin/env python3
"""
Permission hook script for Claude Code.

This script is invoked by Claude's PermissionRequest hook system.
It forwards permission requests to the Slack bot via FIFO and returns
the bot's decision to Claude.

Usage:
  This script receives JSON on stdin and outputs JSON to stdout.
  It communicates with the bot via FIFOs in the .clod directory.
"""

import json
import os
import sys

# FIFO paths relative to .clod-runtime directory
FIFO_REQUEST = "permission_request.fifo"
FIFO_RESPONSE = "permission_response.fifo"


def find_runtime_dir():
    """Find the .clod-runtime directory by walking up from cwd."""
    cwd = os.getcwd()
    path = cwd
    while path != "/":
        runtime_path = os.path.join(path, ".clod-runtime")
        if os.path.isdir(runtime_path):
            return runtime_path
        path = os.path.dirname(path)
    # Fallback to cwd/.clod-runtime
    return os.path.join(cwd, ".clod-runtime")


def main():
    # Read input from stdin
    try:
        input_data = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"Error: Invalid JSON input: {e}", file=sys.stderr)
        sys.exit(1)

    # Find .clod-runtime directory
    runtime_dir = find_runtime_dir()
    request_fifo = os.path.join(runtime_dir, FIFO_REQUEST)
    response_fifo = os.path.join(runtime_dir, FIFO_RESPONSE)

    # Check if FIFOs exist (bot is listening)
    if not os.path.exists(request_fifo) or not os.path.exists(response_fifo):
        # No bot listening - exit without output to let user decide
        print("Permission FIFOs not found, falling back to default behavior", file=sys.stderr)
        sys.exit(0)

    # Extract relevant fields for the bot
    request = {
        "session_id": input_data.get("session_id", ""),
        "tool_name": input_data.get("tool_name", ""),
        "tool_input": input_data.get("tool_input", {}),
        "tool_use_id": input_data.get("tool_use_id", ""),
        "permission_mode": input_data.get("permission_mode", ""),
        "cwd": input_data.get("cwd", ""),
    }

    try:
        # Write request to FIFO (blocks until bot reads)
        with open(request_fifo, "w") as f:
            f.write(json.dumps(request) + "\n")
            f.flush()

        # Read response from FIFO (blocks until bot writes)
        with open(response_fifo, "r") as f:
            response_line = f.readline().strip()
            if not response_line:
                print("Empty response from bot", file=sys.stderr)
                sys.exit(0)

            response = json.loads(response_line)

    except (IOError, OSError) as e:
        print(f"FIFO communication error: {e}", file=sys.stderr)
        sys.exit(0)
    except json.JSONDecodeError as e:
        print(f"Invalid response JSON: {e}", file=sys.stderr)
        sys.exit(0)

    # Format output for Claude's hook system
    output = {
        "hookSpecificOutput": {
            "hookEventName": "PermissionRequest",
            "decision": {
                "behavior": response.get("behavior", "deny"),
            }
        }
    }

    # Add optional message for deny
    if response.get("message"):
        output["hookSpecificOutput"]["decision"]["message"] = response["message"]

    print(json.dumps(output))
    sys.exit(0)


if __name__ == "__main__":
    main()
