"""Derive and compile an isolated stricter AF_UNIX probe profile."""
import hashlib

from profile_policy import MARKER, normalize
import parser_checks
from parser_checks import compile_profile

UNIX_ALLOW = b"  network unix stream,\n"


def candidate_bytes(template):
    original = normalize(template)
    if original.count(UNIX_ALLOW) != 1:
        raise ValueError("Chart profile must contain exactly one expected AF_UNIX allowance")
    candidate = original.replace(UNIX_ALLOW, b"", 1)
    name = "sandbox-fuse-unix-probe-" + hashlib.sha256(candidate).hexdigest()
    if candidate.count(MARKER.encode()) != 1:
        raise ValueError("candidate profile has an invalid profile-name marker")
    return name, candidate.replace(MARKER.encode(), name.encode())


def compile_candidate(parser, profile, features, output):
    return compile_profile(parser, profile, output, features=features, policy_features=features)
