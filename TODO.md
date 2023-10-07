# TODO

### Features

- [ ] Split xtemplate from caddy so it can be used standalone
    - [ ] Integrate a static file server based on `caddy.caddyhttp.file_server`
    - [ ] Split into separate packages `xtemplate` and `xtemplate/caddy`, rename repo to `xtemplate` (?)

### Demos

- [ ] Demonstrate how to do auth with xtemplate
    - [ ] [forward_auth](https://caddyserver.com/docs/caddyfile/directives/forward_auth#forward-auth) / [Trusted Header SSO](https://www.authelia.com/integration/trusted-header-sso/introduction/)
- [ ] Demonstrate how to integrate with [caddy-git](https://github.com/greenpau/caddy-git) for zero-CI app deployments


# BACKLOG

- [ ] Client side auto reload
- [ ] Investigate integrating into another web framework (gox/gin etc)
- [ ] Document how to use standalone
- [ ] Demo how to use standalone
- [ ] SSE https://thedevelopercafe.com/articles/server-sent-events-in-go-595ae2740c7a
- [ ] NATS https://github.com/nats-io/nats.go


# DONE

- [x] Make extrafuncs an array
- Split xtemplate from caddy so it can be used standalone
    - [x] Split watcher into a separate component
    - [x] Isolate caddy integration into one file