module holepunch.to/bare_rpc_golang/example

go 1.25.6

replace holepunch.to/compactencoding => ../../compact-encoding-golang

replace holepunch.to/bare_rpc => ../

require (
	holepunch.to/bare_rpc v0.0.0-00010101000000-000000000000
	holepunch.to/compactencoding v0.0.0-00010101000000-000000000000
)
