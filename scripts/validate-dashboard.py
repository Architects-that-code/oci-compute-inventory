"""Validate the portable dashboard contract against the collector metric names."""
import json
import re
from pathlib import Path

root = Path(__file__).resolve().parents[1]
dashboard = json.loads((root / "dashboards/oci-global-compute-inventory.json").read_text())
assert dashboard["uid"] == "oci-global-compute-inventory"
variables = {v["name"] for v in dashboard["templating"]["list"]}
assert {"region", "compartment"} <= variables
used = set(re.findall(r"\boci_compute_[a-z_]+", json.dumps(dashboard)))
source = (root / "collector/main.go").read_text()
assert used and all(metric in source for metric in used), used
print(f"Dashboard JSON and {len(used)} collector metric references validated")
