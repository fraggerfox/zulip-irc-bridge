# NixOS module for zulip-irc-bridge.
#
# The bridge's TOML config is rendered from `settings` into the nix
# store — safe because the config format keeps secrets out of the file
# via *_file indirection. Secrets are wired through systemd
# LoadCredential: each `credentials.<name> = /path` entry is exposed to
# the service at /run/credentials/zulip-irc-bridge.service/<name>, read
# as root at service start (source files can stay root-owned 0400).
#
# `package` defaults to this flake's build; the flake wires that in via
# a specialArgs-free trick: the module is a function returning a module
# so the flake can inject its own package set.
{ selfPackages }:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.zulip-irc-bridge;
  settingsFormat = pkgs.formats.toml { };
  configFile = settingsFormat.generate "zulip-irc-bridge.toml" cfg.settings;
in
{
  options.services.zulip-irc-bridge = {
    enable = lib.mkEnableOption "the Zulip <-> IRC bridge";

    package = lib.mkOption {
      type = lib.types.package;
      default = selfPackages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "zulip-irc-bridge.packages.\${system}.default";
      description = "The zulip-irc-bridge package to run.";
    };

    settings = lib.mkOption {
      type = settingsFormat.type;
      default = { };
      description = ''
        Bridge configuration, rendered to TOML. See config.example.toml
        in the zulip-irc-bridge repository for the schema. Do not put
        secrets here — the file lands in the world-readable nix store;
        use the `*_file` variants pointing at credential paths (see
        {option}`services.zulip-irc-bridge.credentials`).
      '';
      example = lib.literalExpression ''
        {
          zulip = {
            site = "https://zulip.example.com";
            email = "irc-bot@zulip.example.com";
            api_key_file = "/run/credentials/zulip-irc-bridge.service/zulip_api_key";
          };
          irc = {
            server = "irc.libera.chat";
            nick = "example_bridge";
          };
          mapping = [
            {
              channel = "##example";
              stream = "irc-example";
              topic = "general chat";
            }
          ];
        }
      '';
    };

    credentials = lib.mkOption {
      type = lib.types.attrsOf lib.types.path;
      default = { };
      description = ''
        Secret files exposed to the service via systemd LoadCredential.
        Each entry `name = /path/to/secret` becomes readable by the
        bridge at {file}`/run/credentials/zulip-irc-bridge.service/name`.
        Source files are read as root at service start, so sops-nix /
        agenix secrets can stay root-owned.
      '';
      example = lib.literalExpression ''
        { zulip_api_key = config.sops.secrets.zulip_irc_bot_key.path; }
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.zulip-irc-bridge = {
      description = "Zulip <-> IRC bridge";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];

      serviceConfig = {
        Type = "notify";
        # Validate the config before replacing a running instance; a
        # broken deploy fails here instead of taking the bridge down.
        ExecStartPre = "${lib.getExe cfg.package} -config ${configFile} -check";
        ExecStart = "${lib.getExe cfg.package} -config ${configFile}";

        LoadCredential = lib.mapAttrsToList (name: path: "${name}:${path}") cfg.credentials;

        # Reconnect pacing: never hammer the IRC server after repeated
        # failures (Libera bans reconnect floods).
        Restart = "on-failure";
        RestartSec = "10s";
        RestartSteps = 10;
        RestartMaxDelaySec = "30min";

        # Sandboxing: the bridge needs outbound network and nothing else.
        DynamicUser = true;
        NoNewPrivileges = true;
        PrivateTmp = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        ProtectClock = true;
        ProtectHostname = true;
        ProtectProc = "invisible";
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX" # sd_notify
        ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [
          "@system-service"
          "~@privileged"
        ];
        CapabilityBoundingSet = "";
        UMask = "0077";
      };
    };
  };
}
