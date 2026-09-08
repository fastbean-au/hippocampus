"""The package version.

Deliberately a placeholder in the source tree. Item 64's rule is that the client publishes from
the same tag the contract ships from, so the release workflow stamps the tag in here before
building and refuses to publish if the stamp did not take - a hand-maintained number is the thing
that would let the package and the contract claim different versions of the same release.
"""

__version__ = "0.0.0.dev0"
