#!/usr/bin/env ruby
# Test-only: never change production scheduling or any identity/storage fields.
require 'yaml'

begin
  namespace = ENV.fetch('SANDBOX_SENTINEL_TEST_NAMESPACE')
  release = ENV.fetch('SANDBOX_SENTINEL_TEST_RELEASE')
  raise 'invalid scope' unless namespace.match?(/\Asandbox-cce-sentinel-test-[a-f0-9]{12}\z/) && release == 'sandbox-fuse'
  raise 'unexpected arguments' unless ARGV.empty?

  input = STDIN.read(4 * 1024 * 1024 + 1)
  raise 'oversized input' if input.bytesize > 4 * 1024 * 1024
  docs = YAML.parse_stream(input).children.map do |doc|
    stream = Psych::Nodes::Stream.new
    stream.children << doc
    YAML.safe_load(stream.to_yaml, aliases: false)
  end.compact
  raise 'invalid documents' unless docs.all? { |doc| doc.is_a?(Hash) && doc['metadata'].is_a?(Hash) }
  raise 'cross-namespace input' unless docs.all? { |doc| doc['metadata']['namespace'].nil? || doc['metadata']['namespace'] == namespace }
  targets = docs.select do |doc|
    doc['apiVersion'] == 'apps/v1' && doc['kind'] == 'StatefulSet' &&
      doc['metadata']['name'] == "#{release}-redis-sentinel" && doc['metadata']['namespace'] == namespace
  end
  raise 'ambiguous target' unless targets.size == 1
  spec = targets[0].fetch('spec')
  labels = {'app' => 'sandbox-redis-sentinel', 'release' => release}
  raise 'invalid topology' unless spec['replicas'] == 3 && spec['podManagementPolicy'] == 'Parallel' && spec.fetch('selector')['matchLabels'] == labels
  anti = spec.fetch('template').fetch('spec').fetch('affinity').fetch('podAntiAffinity')
  raise 'unexpected affinity' unless anti.keys == ['requiredDuringSchedulingIgnoredDuringExecution']
  terms = anti.fetch('requiredDuringSchedulingIgnoredDuringExecution')
  expected = {'topologyKey' => 'kubernetes.io/hostname', 'labelSelector' => {'matchLabels' => labels}}
  raise 'unexpected term' unless terms == [expected]

  anti.delete('requiredDuringSchedulingIgnoredDuringExecution')
  anti['preferredDuringSchedulingIgnoredDuringExecution'] = [{'weight' => 100, 'podAffinityTerm' => terms[0]}]
  # Serialize all documents before writing, so even a late failure emits no partial manifest.
  output = docs.map(&:to_yaml).join
  STDOUT.write(output)
rescue StandardError
  warn 'single-node Sentinel test renderer rejected input'
  exit 1
end
