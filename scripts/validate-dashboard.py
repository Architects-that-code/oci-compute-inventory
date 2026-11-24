"""Validate portable dashboard links and metric references."""
import json
import re
from pathlib import Path

root = Path(__file__).resolve().parents[1]
paths = sorted((root / "dashboards").glob("*.json"))
dashboards = {path.stem: json.loads(path.read_text()) for path in paths}
assert {
    "oci-global-compute-inventory",
    "oci-global-compute-inventory-map",
    "oci-compute-instance-operations",
} <= dashboards.keys()

source = "\n".join(path.read_text() for path in (root / "collector").glob("*.go") if not path.name.endswith("_test.go"))
external_metrics = {"oci-global-compute-inventory-map": {"oci_region_info"}}
for uid, dashboard in dashboards.items():
    assert dashboard["uid"] == uid, uid
    variables = {v["name"] for v in dashboard["templating"]["list"]}
    assert {"region", "compartment"} <= variables, uid
    panel_ids = [panel["id"] for panel in dashboard["panels"]]
    assert len(panel_ids) == len(set(panel_ids)), uid
    for link in dashboard.get("links", []):
        if link["url"].startswith("/d/"):
            assert link["url"].split("/")[2] in dashboards, (uid, link["url"])
    used = set(re.findall(r"\boci_[a-z_]+\b", json.dumps(dashboard)))
    external = external_metrics.get(uid, set())
    assert external <= used, uid
    unknown = {metric for metric in used - external if f'"{metric}"' not in source}
    assert used and not unknown, (uid, unknown)
    print(f"{uid}: {len(used)} metric references and dashboard links validated")
