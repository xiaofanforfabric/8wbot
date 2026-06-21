Disconnecting: org.geysermc.mcprotocollib.network.event.session.DisconnectingEvent@5340477f
Disconnected: org.geysermc.mcprotocollib.network.event.session.DisconnectedEvent@7eecb5b8
Kick reason: <not available>

[Incubating] Problems report is available at: file:///I:/8wbot/build/reports/problems/problems-report.html

Deprecated Gradle features were used in this build, making it incompatible with Gradle 9.0.

You can use '--warning-mode all' to show the individual deprecation warnings and determine if they come from your own scripts or plugins.

For more on this, please refer to https://docs.gradle.org/8.13/userguide/command_line_interface.html#sec:command_line_warnings in the Gradle documentation.

BUILD SUCCESSFUL in 43s
2 actionable tasks: 1 executed, 1 up-to-date
PS I:\8wbot> 

Add-Type -AssemblyName System.IO.Compression.FileSystem
$jarPath = 'G:\Android\Gradle\caches\modules-2\files-2.1\org.geysermc.mcprotocollib\protocol\1.21.4-SNAPSHOT\7f7bc38bfc087c0bf2356d6b7d197361ee05321\protocol-1.21.4-SNAPSHOT.jar'
$zip = [System.IO.Compression.ZipFile]::OpenRead($jarPath)
$zip.Entries | ForEach-Object { $_.FullName } | Where-Object { $_ -match 'Serverbound' -or $_ -match 'serverbound' -or $_ -match 'Move' -or $_ -match 'move' -or $_ -match 'Player' -or $_ -match 'player' } | Select-Object -Unique
$zip.Dispose()
