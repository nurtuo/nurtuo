// SPDX-License-Identifier: Apache-2.0
using Nurtuo.Bridge;
using Nurtuo.Bridge.Examples;

// dotnet run --project Nurtuo.Bridge.Examples          (the runtime sets the NURTUO_BRIDGE_* variables)
return await BridgeHost.RunAsync(new EchoConnector());
