# Shared name matching: exact, or a leading/trailing `*` wildcard.
package axda.match

matches(pattern, name) if pattern == name

matches(pattern, _) if pattern == "*"

matches(pattern, name) if {
	endswith(pattern, "*")
	not startswith(pattern, "*")
	startswith(name, trim_suffix(pattern, "*"))
}

matches(pattern, name) if {
	startswith(pattern, "*")
	not endswith(pattern, "*")
	endswith(name, trim_prefix(pattern, "*"))
}

matches(pattern, name) if {
	startswith(pattern, "*")
	endswith(pattern, "*")
	count(pattern) > 2
	contains(name, trim_suffix(trim_prefix(pattern, "*"), "*"))
}

any_matches(patterns, name) if {
	some p in patterns
	matches(p, name)
}
