# Utilization aware Batch size adjustments [[Phase 4 Design]](../proposal.md#4-1-dnc-rc-dynamically-adjusts-the-batch)

The approach of setting the Batch directly to 1 when an unexhausted subnet exceeds some exhaustion threshold and setting it back to $B_0$ when an exhausted subnet falls below an unexhaustion threshold is typically effective but may lead to fluttering of the Batch size as the subnet toggles between exhaustion statuses.

For any Subnet capacity $C$, exhaustion thresholds $T_u$ and $T_l$, and a Scaler value of $B_0$, there is some mininum quantity of Nodes $N$ which will oscillate in exhaustion state as the Utilization increases.

The number of IPs freed by changing the Batch size from $B_0$ to $B_1$(=1) is:

$$
freed = N \cdot \left(B_0-B_1\right)
$$

> Note: this neglects the min free fraction $mf$ to simplify the math. Including it scales the result, but does not change the conclusion.

The gap between the upper exhaustion threshold (which triggers exhaustion, and the reclamation of IPs), and the lower exhaustion threshold (which indicates unexhaustion and reallocation of IPs), is:

$$
gap = C \cdot \left(T_u - T_l\right)
$$

And if $freed > gap$, the exhausted subnet will reclaim enough IPs that it will immediately fall back below the unexhaustion threshold, leading to it reallocating IPs and immediately becoming exhausted again, and looping.

The critical Node count for a given Subnet Capacity can thus be calculated as: 

$$
N \ge \frac{C \cdot \left(T_u - T_l\right)}{\left(B_0-B_1\right)}
$$

For a `/22` Subnet, with the default $90\%$ and $50\%$ thresholds and $B=16$:

$$
\displaylines{
    N \ge \frac{1024 \cdot \left(.9 - .5\right)}{\left(16-1\right)}\\
    \quad\\
    N \ge 27.3\\
}
$$

Meaning that if the Cluster has 28 or more Nodes and reaches a Subnet Utilization $\ge90\%$ it will be stuck in a exhaustion/unexhaustion loop.

The solution to this must necessarily account for the number of Nodes $N$ and the "freeable IPs" from any $B_i$ to $B_{i+1}$ to prevent oscillation between exhausted and unexhausted states.

#### Note: everything that follows is based on the fundamental assumption that in Dynamic IP allocation, we want to give each Node as many IPs as possible so that they are available for Pod scheduling immediately, but not so many that we consume the whole Subnet and other Nodes are prevented from getting initial or additional IPs. 
#### In other words: find a general solution to minimize the time between Pod scheduling and Pod IP assignment by optimizing the IP distribution.

## Static level distribution
In an perfect world, we could statically distribute the entire subnet evenly to all Nodes in the cluster once (or each time a Node is added/removed). This naive distribution would look like: 

$$
B=\frac{C}{N}
$$

Unfortunately, this isn't realistic for several reasons:
1) Nodes may have $maxPods \gt B$, meaning that after the inital distribution of IPs, Nodes may have more Pods scheduled than IPs 
2) No IPs are left in the Subnet for other VNET features like a NAT Gateway, LoadBalancer, or even a VM created out-of-band but in the same Subnet
3) B must be recomputed when a Node is added, and IPs must be reclaimed from existing Nodes, _before_ a new Node will be able to allocate IPs out of the Subnet.

If we define some "subnet safety factor" $X_{ssf} \lt 1$, we can use it to protect some amount of capacity in the Subnet which will not be initially distributed to Nodes:

$$ 
B = \frac{X_{ssf} \cdot C}{N}
$$

If we take $X_{ssf}=0.9$, we can see that B will be reduced by the factor: $\\: B = \frac{0.9 \cdot C}{N}$

This protects some subnet capacity for other use, which begins to address point (2) above, and may in some scenarios address point (3) - but we can do better. 

If we make $X_{ssf}$ a scaling factor based on $N$, we can fully address (3) (and (2)), by making the protected capacity of the subnet some fraction of the total Node count. By moving the scaling factor to the denominator, we protect "some fraction of the Node count"'s worth of IPs in the Subnet. Let $X_{nsf}>1$ and rearrange the equation to:

$$ 
B=\frac{C}{X_{nsf} \cdot N}
$$


If we take $X_{nsf}=2$, then: $\\: B=\frac{C}{2 \cdot N} \\:$ and we will calculate B such that when every Node has received $B$ IPs, there are $B \cdot N$ IPs left in the Subnet - sufficient IPs for every Node to request one additional $B$.

In this way we address (2) as best as we can, and (3) partially: some IPs are always left in the Subnet, and they are sufficient for any Node to request an additional Batch of IPs. However, (1) has gotten worse - we are allocating every Node even fewer IPs per Batch and it is more likely that they will have more Pods scheduled than IPs available.

To fully address (1), we will need to incorporate the Subnet Utilization $U$. 

Before we move to that, we can make one more convenience improvement to this equation. Since Subnet Capacities are `base2`, we should align the Batch size to the nearest lesser base2 value. This will have the effect that there will be some multiple of $B$ which evenly divides the Subnet, allowing 100% allocation of the Subnet IPs. For example, for a `/22` Subnet with `1024` IPs, if $B$ is calculated as 20, we should use $B=16$ so that $64 \cdot B = 1024$

It is trivial to calculate "previous base2" by taking the $\log_2$ and rounding down, then raising 2 to the result. If we call this quantity $\beta$:

$$
\beta = \left\lfloor \log_2 \left( \frac{C}{X \cdot N} \right) \right\rfloor
$$

then

$$
B = 2^\beta
$$


## Utilization-based dynamic distribution

To distribute IPs amongst the Nodes in the Cluster with maximal efficiency, that distribution must take in to account the Utilization of the Subnet. As the Utilization increases, the Batch size must be decreased, and as Utilization decreases, the Batch size may be increased. In this way, Nodes retain the maximum number of IPs allocated to them as possible according to the constraints of the Subnet.

Because decreasing the Batch size will decrease the Utilization, and increasing the Batch size will increase the Utilization, there must be different thresholds for changing the Batch size when the Utilization is increasing vs when it is decreasing. 

Consider the Utilization gradient $U|_0^C$. As discussed in the previous section, the ceiling of the Utilization for any $B$ is $U_i = C - N \cdot B_i$.
```ascii
Batch size B vs Subnet Utilization U

     B
      |
  B0..|________________________________
      |                                |
      |                                |
      |                                |
  B1..|................................|________________
      |                                .                |
  B2..|.................................................|________
  B3..|..........................................................|____  ->
     _|_____________________________________________________________________ 
     0|                              U0               U1       U2       U->C  
                                 C-N*B0           C-N*B1   C-N*B2       ...
```

For the Utilization $U_i$ where:

$$
U_i = \\{ U = 0, \quad\dotsi\quad U_c = C \\}
$$

the Utilization thresholds that trigger changes in the Batch sized are based on the direction that the Utilization is moving:

$$
U_i = \left( U_0 = 0 \left|\frac
{--\triangleright--\triangleright--\triangleright-\dotsi--\triangleright--\triangleright}
{-\triangleleft--\triangleleft--\triangleleft--\dotsi-\triangleleft--\triangleleft-}
\right| U_c = C \right)
$$

 ```ascii
Batch size B vs Subnet Utilization U

     B
      |
  B0..|_______________>________________
      |                                |
      |                                |
      |                                |
  B1..|_______________<________________|________>_______
      |                                |                |
  B2..|................................|________<_______|____>___
  B3..|.................................................|____<___|_>__  ->
     _|_____________________________________________________________________ 
     0|                              U0               U1       U2       U->C  
                                 C-N*B0           C-N*B1   C-N*B2       ... 
```


First, we will examine the ascending scenario (Subnet Utilization is increasing): $\Delta U = U_i - U_{i-1}> 0$

For every $B_i$ where $B_i=\frac{1}{2}B_{i-1}$, the minimum Subnet Utilization is $C - N \cdot B_i$. Therefore, given any Utilization $U$, we can solve for the _maximum_ $B_i$.

In the general case, we can see that:

$$
C - N \cdot B_{i} \le U \lt C - N \cdot B_{i+1}
$$

and, because we decrease $B$ by powers of 2, we know that:

$$
B_{i+1}=\frac{1}{2}B_{i}=B_0 \cdot 2^{-i}
$$

Therefore we can simplify in terms of $B_i$:

$$
\displaylines{
    C - N \cdot B_{i} \lt U \le C - N \cdot B_{i+1} \\
    \quad\\
    C - N \cdot B_{i} \lt U \le C - N \cdot \frac{1}{2}B_{i} \\
    \quad\\
    - N \cdot B_{i} \lt U - C \le - N \cdot \frac{1}{2}B_{i} \\
    \quad\\
    \frac{1}{2}B_{i} \le \frac{C - U}{N} \lt B_{i}  \\
    \quad\\
}
$$

Further, since $B_i$ is based on some $B_0$ reduced by subsequent factors of 2, we can redefine $B_i = B_0 \cdot 2^{-i}$.

To solve for $B_i$, we need to establish a $B_0$. Since we want our distribution to approach the efficiency of the idealized "Static Level Distribution", above, we take $B_0 = 2^\beta$ as the limit of our IP distribution.

Substituting through:

$$
\displaylines{
    \frac{1}{2}B_{i} \le \frac{C - U}{N} \lt B_{i}  \\
    \quad\\
    \frac{1}{2} \cdot B_0 \cdot 2^{-i} \le \frac{C - U}{N} \lt B_0 \cdot 2^{-i} \\
    \quad\\
    \frac{1}{2} \cdot 2^{\beta} \cdot 2^{-i} \le \frac{C - U}{N} \lt  2^{\beta} \cdot 2^{-i} \\
    \quad\\
    \log_2 \left( \frac{1}{2} \cdot 2^{\beta} \cdot 2^{-i} \le \frac{C - U}{N} \lt  2^{\beta} \cdot 2^{-i} \right)\\
    \quad\\
    \beta -i -1 \le \log_2 \left( \frac{C - U}{N} \right) \lt \beta -i\\
    \quad\\
}
$$

Rearranging, we can now bound $i$ as:

$$
\displaylines{
    \beta -i -1 \le \log_2 \left( \frac{C - U}{N} \right) \lt \beta -i\\
    \quad\\
    \beta -1 \le \log_2 \left( \frac{C - U}{N} \right) + i \lt \beta\\
    \quad\\
    \beta - \log_2 \left( \frac{C - U}{N} \right) \le i \lt \beta - \log_2 \left( \frac{C - U}{N} \right) + 1\\
    \quad\\
}
$$



Finally, since we are using $i$ as the threshold of step function we can use the lowest integer value greater than the result of the left side of the inequality.

$$
i = \left\lceil \beta - \log_2 \left( \frac{C - U}{N} \right) \right\rceil
$$

It will later be convenient to define $\lambda = \log_2 \left( \frac{C - U}{N} \right)$ making:

$$
i = \left\lceil \beta - \lambda \right\rceil
$$


---

Since we used the optimal distribution $2^\beta$ to calculate the above, this provides the value for $i$ in $B = 2^{\beta-i}$ (in other words, the number of powers of two below the optimal distribution that $B_i$ should be for any $U$) when $\Delta U \gt 0$, when Utilization is increasing. We also need to consider the scenario where $\Delta U \lt 0$ to prevent a significant potential problem: oscillations.

As the Subnet Utilization increases, $U$ will cross the threshold $U_i$ and the Batch size will change from $B_i \rightarrow B_{i+1}$. As we are ascending, this will halve the Batch size, meaning that $\Delta B \cdot N$ IPs will be released back in to the Subnet. If the release of IPs is sufficient to drop $U$ back below $U_i$, then using a single equation for $B$ would create oscillations where we would hit a threshold, release IPs, drop below the threshold, reallocate IPs, repeat, ... 

To prevent this, for $\Delta U \lt 0$, it is necessary to shift the Batch by one power of two: $B_i = 2^{\beta - i - 1}$. It can be shown that this solution prevents cycles and is convergent for any range of inputs $(U, N)$.

We can redefine $\gamma_\uparrow = i$ and $\gamma_\downarrow = i - 1$ and we have the set of solutions for $B_i$ in increasing and decreasing Utilization scenarios:

$$
B_i = 2 ^ {\beta - \gamma}
\begin{cases}
2^{\beta - \gamma_\uparrow}
& \text{when} \; \Delta U > 0
\\
2^{\beta - \gamma_\downarrow}
& \text{when} \; \Delta U < 0
\\
\end{cases}
$$

---

Fully substituted for knowns, the set of equations for $B$ are:

$$
B = \begin{cases} 
2^{\left\lfloor \log_2 \left( \frac{C}{X_{nsf} \cdot N} \right) \right\rfloor - \left\lceil \left\lfloor \log_2 \left( \frac{C}{X_{nsf} \cdot N} \right) \right\rfloor - \log_2\left(\frac{C-U}{N}\right) \right\rceil} 
& \text{when} \quad \Delta U > 0
\\
2^{\left\lfloor \log_2 \left( \frac{C}{X_{nsf} \cdot N} \right) \right\rfloor - \left\lceil \left\lfloor \log_2 \left( \frac{C}{X_{nsf} \cdot N} \right) \right\rfloor - \log_2\left(\frac{C-U}{N}\right) \right\rceil - 1} 
& \text{when} \quad \Delta U < 0
\\
\end{cases}
$$

In Go: 
```golang
// if delta U is positive, du is 0.
// if delta U is negative, du is 1.
// this allows us to do this calculation in one step with no conditionals.
func b(c, x, n, u, du float64) float64 {
	beta := math.Floor(math.Log2(c / (x * n)))
	lambda := math.Log2((c - u) / n)
	gamma := math.Ceil(beta-lambda) - du
	return math.Exp2(beta - gamma)
}
```

