#!/usr/bin/env python3
"""
MCP server for permission prompts.

This server implements a tool that can be used with Claude's --permission-prompt-tool flag.
It forwards permission requests to the Slack bot via FIFO and returns the bot's decision.

Usage:
  claude -p --permission-prompt-tool mcp__permission__request_permission \
    --mcp-config mcp_config.json \
    "your prompt"
"""

import json
import os
import sys
from typing import Any


# FIFO paths relative to .clod-runtime directory
FIFO_REQUEST = "permission_request.fifo"
FIFO_RESPONSE = "permission_response.fifo"


def find_runtime_dir() -> str:
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


def log(msg: str) -> None:
    """Log to stderr."""
    print(f"[permission_mcp] {msg}", file=sys.stderr)


def send_response(response: dict) -> None:
    """Send a JSON-RPC response."""
    print(json.dumps(response), flush=True)


def handle_initialize(request_id: Any) -> None:
    """Handle the initialize request."""
    send_response({
        "jsonrpc": "2.0",
        "id": request_id,
        "result": {
            "protocolVersion": "2024-11-05",
            "capabilities": {
                "tools": {}
            },
            "serverInfo": {
                "name": "permission-mcp",
                "version": "1.0.0"
            }
        }
    })


def handle_tools_list(request_id: Any) -> None:
    """Handle the tools/list request."""
    send_response({
        "jsonrpc": "2.0",
        "id": request_id,
        "result": {
            "tools": [
                {
                    "name": "request_permission",
                    "description": "Request permission from the user for a tool operation",
                    "inputSchema": {
                        "type": "object",
                        "properties": {
                            "tool_name": {
                                "type": "string",
                                "description": "Name of the tool requesting permission"
                            },
                            "input": {
                                "type": "object",
                                "description": "Input parameters for the tool"
                            }
                        },
                        "required": ["tool_name", "input"]
                    }
                }
            ]
        }
    })


def handle_tool_call(request_id: Any, params: dict) -> None:
    """Handle a tools/call request for permission."""
    tool_name = params.get("name", "")
    arguments = params.get("arguments", {})

    if tool_name != "request_permission":
        send_response({
            "jsonrpc": "2.0",
            "id": request_id,
            "error": {
                "code": -32601,
                "message": f"Unknown tool: {tool_name}"
            }
        })
        return

    # Extract permission request details
    perm_tool_name = arguments.get("tool_name", "unknown")
    perm_input = arguments.get("input", {})

    log(f"Permission requested for tool: {perm_tool_name}")

    # Find runtime directory with FIFOs
    runtime_dir = find_runtime_dir()
    request_fifo = os.path.join(runtime_dir, FIFO_REQUEST)
    response_fifo = os.path.join(runtime_dir, FIFO_RESPONSE)

    # Check if FIFOs exist
    if not os.path.exists(request_fifo) or not os.path.exists(response_fifo):
        log(f"FIFOs not found at {runtime_dir}, defaulting to deny")
        send_response({
            "jsonrpc": "2.0",
            "id": request_id,
            "result": {
                "content": [
                    {
                        "type": "text",
                        "text": json.dumps({
                            "behavior": "deny",
                            "message": "Permission system not available"
                        })
                    }
                ]
            }
        })
        return

    try:
        # Build request for the Slack bot
        request = {
            "tool_name": perm_tool_name,
            "tool_input": perm_input,
        }

        log(f"Writing to request FIFO: {request_fifo}")

        # Write request to FIFO (blocks until bot reads)
        with open(request_fifo, "w") as f:
            f.write(json.dumps(request) + "\n")
            f.flush()

        log(f"Reading from response FIFO: {response_fifo}")

        # Read response from FIFO (blocks until bot writes)
        with open(response_fifo, "r") as f:
            response_line = f.readline().strip()
            if not response_line:
                log("Empty response from bot")
                behavior = "deny"
                message = "Empty response from permission system"
            else:
                response = json.loads(response_line)
                behavior = response.get("behavior", "deny")
                message = response.get("message", "")

        log(f"Got permission response: behavior={behavior}")

        # Return the result in the format Claude expects:
        # Allow: {"behavior": "allow", "updatedInput": {...}}
        # Deny: {"behavior": "deny", "message": "..."}
        if behavior == "allow":
            result_data = {
                "behavior": "allow",
                "updatedInput": perm_input  # Pass through the original input
            }
        else:
            result_data = {
                "behavior": "deny",
                "message": message or "Permission denied by user"
            }

        send_response({
            "jsonrpc": "2.0",
            "id": request_id,
            "result": {
                "content": [
                    {
                        "type": "text",
                        "text": json.dumps(result_data)
                    }
                ]
            }
        })

    except Exception as e:
        log(f"Error handling permission request: {e}")
        send_response({
            "jsonrpc": "2.0",
            "id": request_id,
            "result": {
                "content": [
                    {
                        "type": "text",
                        "text": json.dumps({
                            "behavior": "deny",
                            "message": f"Permission system error: {e}"
                        })
                    }
                ]
            }
        })


def main():
    """Main MCP server loop."""
    log("Starting permission MCP server")

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue

        try:
            request = json.loads(line)
        except json.JSONDecodeError as e:
            log(f"Invalid JSON: {e}")
            continue

        method = request.get("method", "")
        request_id = request.get("id")
        params = request.get("params", {})

        log(f"Received method: {method}")

        if method == "initialize":
            handle_initialize(request_id)
        elif method == "notifications/initialized":
            # Notification, no response needed
            pass
        elif method == "tools/list":
            handle_tools_list(request_id)
        elif method == "tools/call":
            handle_tool_call(request_id, params)
        else:
            log(f"Unknown method: {method}")
            if request_id is not None:
                send_response({
                    "jsonrpc": "2.0",
                    "id": request_id,
                    "error": {
                        "code": -32601,
                        "message": f"Method not found: {method}"
                    }
                })


if __name__ == "__main__":
    main()
