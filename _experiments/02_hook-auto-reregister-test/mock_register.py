"""
Mock replacement for openai_register3.py.

Instead of actually registering an OpenAI account, it:
  1. Creates an output/ directory
  2. Generates a fake token_*.json with realistic structure
  3. Prints simulated registration logs

Usage: python3 mock_register.py --once [--proxy ...]
"""

import json
import os
import sys
import uuid
import time
import argparse


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--once", action="store_true")
    parser.add_argument("--proxy", type=str, default="")
    args = parser.parse_args()

    script_dir = os.path.dirname(os.path.abspath(__file__))
    output_dir = os.path.join(script_dir, "output")
    os.makedirs(output_dir, exist_ok=True)

    fake_token = f"token_{uuid.uuid4().hex[:12]}"
    filename = f"{fake_token}.json"
    filepath = os.path.join(output_dir, filename)

    token_data = {
        "email": f"test_{uuid.uuid4().hex[:6]}@proton.me",
        "password": "MockP@ssw0rd123",
        "access_token": f"eyJ-mock-{uuid.uuid4().hex}",
        "refresh_token": f"rt-mock-{uuid.uuid4().hex}",
        "type": "codex",
        "created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "mock": True
    }

    print("[mock-register] ============================================")
    print(f"[mock-register] Simulating OpenAI registration (--once)")
    if args.proxy:
        print(f"[mock-register] Using proxy: {args.proxy}")
    print(f"[mock-register] Generating token: {filename}")

    with open(filepath, "w") as f:
        json.dump(token_data, f, indent=2)

    print(f"[mock-register] Token saved to: {filepath}")
    print("[mock-register] Registration complete (mock)")
    print("[mock-register] ============================================")


if __name__ == "__main__":
    main()
