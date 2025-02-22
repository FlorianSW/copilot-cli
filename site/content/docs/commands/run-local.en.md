# run local
```console
$ copilot run local [flags]
```

## What does it do?
`copilot run local` runs a workload locally.

## What are the flags?
```
  -a, --app string                        Name of the application. (default "playground")
  -e, --env string                        Name of the environment.
      --env-var-override stringToString   Optional. Override environment variables passed to containers.
                                          Format: [container]:KEY=VALUE. Omit container name to apply to all containers. (default [])
  -h, --help                              help for run
  -n, --name string                       Name of the service or job.
      --port-override list                Optional. Override ports exposed by service. Format: <host port>:<service port>.
                                          Example: --port-override 5000:80 binds localhost:5000 to the service's port 80. (default [])
```

## Examples
Runs the service "mysvc" in environment "test" locally.
```console
$ copilot run local --name mysvc --env test
```