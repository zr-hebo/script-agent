"""Run with Script-Agent; prepare/post_run are optional and omitted here."""


def run(params):
    cluster_uuid = params.get("cluster_uuid")
    dry_run = params.get("dry_run", True)
    if not isinstance(cluster_uuid, str) or not cluster_uuid:
        raise ValueError("cluster_uuid is required")
    if not isinstance(dry_run, bool):
        raise ValueError("dry_run must be a boolean")

    # Add cluster-specific logic here. This example makes no external changes.
    print(f"start cluster_uuid={cluster_uuid}")
    print(f"processing cluster_uuid={cluster_uuid} dry_run={str(dry_run).lower()}")
    print(f"finished cluster_uuid={cluster_uuid}")
    return {
        "status": "succeeded",
        "data": {"cluster_uuid": cluster_uuid, "dry_run": dry_run},
        "error": None,
    }
