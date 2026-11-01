//go:build windows

package uvc

// psHelperScript is the resident PowerShell helper that carries UVC PTZ
// control operations. See the rationale in client_windows.go: vendor camera
// proxies have been observed crashing the calling process on control calls,
// and the MediaCapture (WinRT) control path — which works on the same device
// — is only reachable from a runtime with a WinRT projection. Windows ships
// PowerShell 5.1 with that projection built in, so the helper is a
// PowerShell process speaking the same line-based JSON protocol the Go
// sub-command used to speak (spawned with -EncodedCommand, UTF-16LE base64).
//
// Protocol (one JSON request per stdin line, one JSON response per stdout
// line): the first request is {"op":"dev","path":...,"name":...}; following
// requests are ops caps/pos/rel/zoom/abs/home. The device binding is
// resolved by friendly-name match against WinRT's own enumeration.
const psHelperScript = `
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Runtime.WindowsRuntime
$null = [Windows.Media.Capture.MediaCapture, Windows.Foundation, ContentType = WindowsRuntime]
$null = [Windows.Devices.Enumeration.DeviceInformation, Windows.Foundation, ContentType = WindowsRuntime]

# 'IAsyncOperation<tick>1' built at runtime (a backtick cannot live in a raw literal)
$asyncOpName = 'IAsyncOperation' + [char]0x60 + '1'
$asTask = ([System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object {
    $_.Name -eq 'AsTask' -and $_.GetParameters().Count -eq 1 -and
    $_.GetParameters()[0].ParameterType.Name -eq $asyncOpName
})[0]
function Await($winRtTask, $resultType) {
    $netTask = $asTask.MakeGenericMethod($resultType).Invoke($null, @($winRtTask))
    $netTask.Wait(-1) | Out-Null
    return $netTask.Result
}
function AwaitAction($winRtAction) {
    $t = ([System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object {
        $_.Name -eq 'AsTask' -and $_.GetParameters().Count -eq 1 -and
        $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncAction'
    })[0].Invoke($null, @($winRtAction))
    $t.Wait(-1) | Out-Null
}

$CC_SET = '{C6E13370-30AC-11D0-A18C-00A0C9118956} '
$script:mc = $null
$script:vdc = $null

function Find-Device([string]$path, [string]$name) {
    $op = [Windows.Devices.Enumeration.DeviceInformation]::FindAllAsync(
        [Windows.Devices.Enumeration.DeviceClass]::VideoCapture)
    $devices = Await $op ([Windows.Devices.Enumeration.DeviceInformationCollection])
    # The configured path is a DirectShow symbolic link; the WinRT Id carries
    # the same PnP identity with a different interface GUID — match on the
    # part before the '{interface}' separator, then fall back to the name.
    $prefix = ''
    if ($path) {
        $idx = $path.IndexOf('{')
        if ($idx -gt 0) { $prefix = $path.Substring(0, $idx).ToLowerInvariant() }
    }
    foreach ($d in $devices) {
        if ($prefix -ne '' -and $d.Id -and $d.Id.ToLowerInvariant().StartsWith($prefix)) { return $d }
        if ($name -and $d.Name -eq $name) { return $d }
    }
    return $null
}

function Init-Media([string]$path, [string]$name) {
    if ($script:mc -ne $null) { return }
    $dev = Find-Device $path $name
    if ($dev -eq $null) { throw 'device not found' }
    $mc = New-Object Windows.Media.Capture.MediaCapture
    $settings = New-Object Windows.Media.Capture.MediaCaptureInitializationSettings
    $settings.StreamingCaptureMode = [Windows.Media.Capture.StreamingCaptureMode]::Video
    $settings.SharingMode = [Windows.Media.Capture.MediaCaptureSharingMode]::SharedReadOnly
    $settings.VideoDeviceId = $dev.Id
    AwaitAction $mc.InitializeAsync($settings)
    $script:mc = $mc
    $script:vdc = $mc.VideoDeviceController
}

function Prop-Key([int]$id) { return $CC_SET + $id }

function Get-Value([int]$id) {
    $result = $script:vdc.GetDeviceProperty((Prop-Key $id))
    if ($null -eq $result) { return 0 }
    $bytes = [byte[]]$result
    if ($bytes.Length -lt 28) { return 0 }
    return [BitConverter]::ToInt32($bytes, 24)
}
function Get-Flags([int]$id) {
    $result = $script:vdc.GetDeviceProperty((Prop-Key $id))
    if ($null -eq $result) { return 0 }
    $bytes = [byte[]]$result
    if ($bytes.Length -lt 32) { return 0 }
    return [BitConverter]::ToInt32($bytes, 28)
}
function Set-Value([int]$id, [int]$value) {
    # Read-modify-write the full KSPROPERTY structure (smaller payloads are
    # rejected with ERROR_MORE_DATA).
    $payload = [byte[]]$script:vdc.GetDeviceProperty((Prop-Key $id))
    [BitConverter]::GetBytes([int32]$value).CopyTo($payload, 24)
    [BitConverter]::GetBytes([int32]2).CopyTo($payload, 28)   # 2 = manual
    $script:vdc.SetDeviceProperty((Prop-Key $id), $payload)
}
function Get-Cap([int]$id) {
    $ctlNames = @{ 0 = 'Pan'; 1 = 'Tilt'; 3 = 'Zoom' }
    $c = $script:vdc.($ctlNames[$id])
    $caps = $c.Capabilities
    return @{
        supported = [bool]$caps.Supported
        value     = [int](Get-Value $id)
        min       = [int]$caps.Min
        max       = [int]$caps.Max
        step      = [int]$caps.Step
        default   = [int]$caps.Default
        relative  = $true   # KS camera controls are absolute; rel is emulated
        absolute  = $true
        auto      = $false
        manual    = $true
    }
}
function Clamp([hashtable]$cap, [int]$v) {
    if ($v -lt $cap.min) { return $cap.min }
    if ($v -gt $cap.max) { return $cap.max }
    return $v
}
function Move-By([int]$pan, [int]$tilt) {
    if ($pan -ne 0) { $c = Get-Cap 0; Set-Value 0 (Clamp $c ($c.value + $pan)) }
    if ($tilt -ne 0) { $c = Get-Cap 1; Set-Value 1 (Clamp $c ($c.value + $tilt)) }
}
function Zoom-Step([int]$delta) {
    $c = Get-Cap 3
    Set-Value 3 (Clamp $c ($c.value + $delta))
}
function Move-Abs($pan, $tilt, $zoom) {
    if ($null -ne $pan) { Set-Value 0 $pan }
    if ($null -ne $tilt) { Set-Value 1 $tilt }
    if ($null -ne $zoom) { Set-Value 3 $zoom }
}
function Home-All {
    foreach ($id in @(0, 1, 3)) {
        $c = Get-Cap $id
        if ($c.supported) { Set-Value $id $c.default }
    }
}
function Emit($obj) {
    [Console]::Out.WriteLine(($obj | ConvertTo-Json -Compress -Depth 5))
    [Console]::Out.Flush()
}

while ($true) {
    $line = [Console]::In.ReadLine()
    if ($null -eq $line) { break }
    if ($line.Trim() -eq '') { continue }
    try {
        $req = $line | ConvertFrom-Json
    } catch {
        Emit @{ ok = $false; error = ('bad request: ' + $_.Exception.Message) }
        continue
    }
    try {
        switch ($req.op) {
            'dev' {
                if (-not $req.path -and -not $req.name) { throw 'device identity required' }
                Init-Media $req.path $req.name
                Emit @{ ok = $true }
            }
            'caps' {
                Init-Media $req.path $req.name
                Emit @{ ok = $true; caps = @{
                    pan  = Get-Cap 0
                    tilt = Get-Cap 1
                    zoom = Get-Cap 3
                } }
            }
            'pos' {
                Init-Media $req.path $req.name
                Emit @{ ok = $true; pos = @{
                    pan  = [int](Get-Value 0)
                    tilt = [int](Get-Value 1)
                    zoom = [int](Get-Value 3)
                } }
            }
            'rel' {
                Init-Media $req.path $req.name
                $p = 0; $t = 0
                if ($null -ne $req.pan) { $p = [int]$req.pan }
                if ($null -ne $req.tilt) { $t = [int]$req.tilt }
                Move-By $p $t
                Emit @{ ok = $true }
            }
            'zoom' {
                Init-Media $req.path $req.name
                $d = 0
                if ($null -ne $req.delta) { $d = [int]$req.delta }
                Zoom-Step $d
                Emit @{ ok = $true }
            }
            'abs' {
                Init-Media $req.path $req.name
                Move-Abs $req.pan $req.tilt $req.zoom
                Emit @{ ok = $true }
            }
            'home' {
                Init-Media $req.path $req.name
                Home-All
                Emit @{ ok = $true }
            }
            default { Emit @{ ok = $false; error = ('unknown op: ' + $req.op) } }
        }
    } catch {
        Emit @{ ok = $false; error = $_.Exception.Message }
    }
}
`
