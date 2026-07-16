#!/usr/bin/env python3
"""Validate and summarize the capability calibration campaign matrix."""
from __future__ import annotations

import argparse
import json
from pathlib import Path

MATRIX = Path(__file__).with_name("benchmark_matrix.json")


def load_matrix(path: Path = MATRIX) -> dict:
    return json.loads(path.read_text())


def validate(matrix: dict) -> list[str]:
    errors: list[str] = []
    campaigns = matrix.get("campaigns", [])
    ids = [campaign.get("id") for campaign in campaigns]
    if len(ids) != len(set(ids)):
        errors.append("campaign ids must be unique")
    required_kinds = {"coding", "noncoding", "multilingual", "review"}
    kinds = {campaign.get("kind") for campaign in campaigns}
    missing = required_kinds - kinds
    if missing:
        errors.append("missing campaign kinds: " + ", ".join(sorted(missing)))
    gate = matrix.get("promotion_gate", {})
    if gate.get("minimum_items_per_cell", 0) < 20:
        errors.append("minimum_items_per_cell must be at least 20")
    if gate.get("minimum_repeats", 0) < 3:
        errors.append("minimum_repeats must be at least 3")
    for campaign in campaigns:
        if not campaign.get("required_metrics"):
            errors.append(f"{campaign.get('id')}: required_metrics is empty")
    return errors


def summarize(matrix: dict) -> dict:
    campaigns = []
    for campaign in matrix["campaigns"]:
        row = {
            "id": campaign["id"],
            "kind": campaign["kind"],
            "purpose": campaign["purpose"],
            "required_metrics": campaign["required_metrics"],
        }
        if "cells" in campaign:
            row["cells"] = len(campaign["cells"])
            row["fixture_required"] = sum(c.get("status") == "fixture_required" for c in campaign["cells"])
        if "domains" in campaign:
            row["domains"] = len(campaign["domains"])
        campaigns.append(row)
    return {"promotion_gate": matrix["promotion_gate"], "campaigns": campaigns}


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--validate", action="store_true")
    args = parser.parse_args()
    matrix = load_matrix()
    errors = validate(matrix)
    if errors:
        print(json.dumps({"valid": False, "errors": errors}, indent=2, ensure_ascii=False))
        raise SystemExit(1)
    output = {"valid": True}
    if not args.validate:
        output.update(summarize(matrix))
    print(json.dumps(output, indent=2, ensure_ascii=False))


if __name__ == "__main__":
    main()
