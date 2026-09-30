local pw = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
local n = #KEYS / 2
local used = {}
local blocked = 0
for i = 1, n do
  local cur = tonumber(redis.call('GET', KEYS[2 * i - 1]) or '0')
  local prev = tonumber(redis.call('GET', KEYS[2 * i]) or '0')
  used[i] = prev * pw + cur
  if blocked == 0 and used[i] + 1 > tonumber(ARGV[1 + 2 * i]) then
    blocked = i
  end
end
if blocked == 0 then
  for i = 1, n do
    local take = tonumber(ARGV[2 + 2 * i])
    if take ~= 0 then
      redis.call('INCRBY', KEYS[2 * i - 1], take)
      redis.call('PEXPIRE', KEYS[2 * i - 1], ttl)
    end
  end
end
local out = {blocked}
for i = 1, n do
  out[i + 1] = tostring(used[i])
end
return out
